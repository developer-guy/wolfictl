package dag

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"

	"github.com/dominikbraun/graph"
	log "github.com/sirupsen/logrus"
	"go.lsp.dev/uri"

	apko "chainguard.dev/apko/pkg/apk/impl"
)

// Graph represents an interdependent set of packages defined in one or more Melange configurations,
// as defined in Packages, as well as upstream repositories and their package indexes,
// as declared in those configurations files. The graph is directed and acyclic.
type Graph struct {
	Graph    graph.Graph[string, Package]
	packages *Packages
	opts     *graphOptions
	byName   map[string][]string // maintains a listing of all known hashes for a given name
}

// packageHash given anything that implements Package, return the hash to be used
// for the node in the graph.
func packageHash(p Package) string {
	return fmt.Sprintf("%s:%s@%s", p.Name(), p.Version(), p.Source())
}

func newGraph() graph.Graph[string, Package] {
	return graph.New(packageHash, graph.Directed(), graph.Acyclic(), graph.PreventCycles())
}

// cycle represents pairs of edges that create a cycle in the graph
type cycle struct {
	src, target string
}

// NewGraph returns a new Graph using the packages, including names and versions, in the Packages struct.
// It parses the packages to create the dependency graph.
// If the list of packages creates a cycle, an error is returned.
// If a package cannot be resolved, an error is returned, unless WithAllowUnresolved is set.
func NewGraph(pkgs *Packages, options ...GraphOptions) (*Graph, error) {
	var opts = &graphOptions{}
	for _, option := range options {
		if err := option(opts); err != nil {
			return nil, err
		}
	}
	g := &Graph{
		Graph:    newGraph(),
		packages: pkgs,
		opts:     opts,
		byName:   map[string][]string{},
	}

	// indexes is a cache of all repositories. Only some might be used for each package.
	var (
		indexes = make(map[string]apko.NamedIndex)
		errs    []error
	)

	// 1. go through each known origin package, add it as a vertex
	// 2. go through each of its subpackages, add them as vertices, with the sub dependent on the origin
	// 3. go through each of its dependencies, add them as vertices, with the origin dependent on the dependency
	for _, c := range pkgs.Packages() {
		version := fullVersion(&c.Package)
		if err := g.addVertex(c); err != nil && !errors.Is(err, graph.ErrVertexAlreadyExists) {
			errs = append(errs, err)
			continue
		}
		for i := range c.Subpackages {
			subpkg := pkgs.Config(c.Subpackages[i].Name, false)
			for _, subpkgVersion := range subpkg {
				if fullVersion(&subpkgVersion.Package) == version {
					continue
				}
				if err := g.addVertex(subpkgVersion); err != nil && !errors.Is(err, graph.ErrVertexAlreadyExists) {
					errs = append(errs, fmt.Errorf("unable to add vertex for %q subpackage %s-%s: %w", c.String(), subpkgVersion.Name(), subpkgVersion.Version(), err))
					continue
				}
				if err := g.Graph.AddEdge(packageHash(subpkgVersion), packageHash(c)); err != nil && !errors.Is(err, graph.ErrEdgeAlreadyExists) {
					// a subpackage always must depend on its origin package. It is not acceptable to have any errors, other than that we already know about that dependency.
					errs = append(errs, fmt.Errorf("unable to add edge for subpackage %q from %s-%s: %w", c.String(), subpkgVersion.Name(), subpkgVersion.Version(), err))
					continue
				}
			}
		}
		// TODO: should we repeat across multiple arches? Use c.Package.TargetArchitecture []string
		var arch = "x86_64"
		// get all of the repositories that are referenced by the package

		var (
			origRepos   = c.Environment.Contents.Repositories
			origKeys    = c.Environment.Contents.Keyring
			repos       []string
			lookupRepos = []apko.NamedIndex{}
		)
		for _, repo := range append(origRepos, opts.repos...) {
			if index, ok := indexes[repo]; !ok {
				repos = append(repos, repo)
			} else {
				lookupRepos = append(lookupRepos, index)
			}
		}
		keyMap := make(map[string][]byte)
		for _, key := range append(origKeys, opts.keys...) {
			b, err := getKeyMaterial(key)
			if err != nil {
				return nil, fmt.Errorf("failed to get key material for %s: %w", key, err)
			}
			// we can have no error, but still no bytes, as we ignore missing files
			if b != nil {
				keyMap[key] = b
			}
		}
		if len(repos) > 0 {
			loadedRepos, err := apko.GetRepositoryIndexes(repos, keyMap, arch)
			if err != nil {
				return nil, fmt.Errorf("unable to load repositories for %s: %w", c.String(), err)
			}
			for _, repo := range loadedRepos {
				indexes[repo.Source()] = repo
				lookupRepos = append(lookupRepos, repo)
			}
		}
		// add our own packages list to the lookupRepos
		localRepo := pkgs.Repository(arch)
		lookupRepos = append(lookupRepos, localRepo)
		resolver := apko.NewPkgResolver(lookupRepos)
		localRepoSource := localRepo.Source()
		for _, buildDep := range c.Environment.Contents.Packages {
			if buildDep == "" {
				errs = append(errs, fmt.Errorf("empty package name in environment packages for %q", c.Package.Name))
				continue
			}
			// need to resolve the package given the available versions
			// this could be in the current packages set, or in one to which it refers
			// 1. look in c.Environments.Contents.Repositories and c.Environments.Contents.Keyring
			// 2. if there are Repositories, download their index
			// 3. try to resolve first in Repositories and then in current packages

			cycle, err := g.addAppropriatePackage(resolver, c, buildDep, localRepoSource)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			// resolve any cycle
			if cycle != nil {
				if err := g.resolveCycle(cycle, buildDep, resolver, localRepoSource); err != nil {
					sp, _ := graph.ShortestPath(g.Graph, cycle.target, cycle.src) //nolint:errcheck // we do not need to check for an error, as we have an error
					log.Errorf("unresolvable cycle: %s -> %s, caused by: %s", cycle.src, cycle.target, strings.Join(sp, " -> "))
					errs = append(errs, err)
					continue
				}
			}
		}
	}
	if errs != nil {
		return nil, fmt.Errorf("unable to build graph:\n%w", errors.Join(errs...))
	}
	return g, nil
}

// addAppropriatePackage adds the appropriate package to the graph, and returns any cycle that was created.
// The c *Configuration is the source package, while the dep represents the dependency.
func (g *Graph) addAppropriatePackage(resolver *apko.PkgResolver, c Package, dep, localRepo string) (*cycle, error) {
	var (
		pkg         Package
		cycleTarget string
	)
	resolved, err := resolver.ResolvePackage(dep)
	switch {
	case (err != nil || len(resolved) == 0) && g.opts.allowUnresolved:
		if err := g.addDanglingPackage(dep, c); err != nil {
			return nil, fmt.Errorf("%s: unable to add dangling package %s: %w", c, dep, err)
		}
	case (err != nil || len(resolved) == 0):
		return nil, fmt.Errorf("%s: unable to resolve dependency %s: %w", c, dep, err)
	default:
		// no error and we had at least one package listed in `resolved`
		for _, r := range resolved {
			// wolfi-dev has a policy not to use a package to fulfull a dependency, if that package is myself.
			// if I depend on something, and the dependency is the same name as me, it must have a lower version than myself
			if r.Version == c.Version() && dep == c.Name() {
				continue
			}
			resolvedSource := r.Repository().IndexUri()
			if resolvedSource == localRepo {
				// it's in our own packages list, so find the package that is an actual match
				configs := g.packages.Config(r.Name, false)
				if len(configs) == 0 {
					return nil, fmt.Errorf("unable to find package %s-%s in local repository", r.Name, r.Version)
				}
				for _, config := range configs {
					if fullVersion(&config.Package) == r.Version {
						pkg = config
						break
					}
				}
				if pkg == nil {
					return nil, fmt.Errorf("unable to find package %s-%s in local repository", r.Name, r.Version)
				}
			} else {
				pkg = externalPackage{r.Name, r.Version, r.Repository().Uri}
			}
			if err := g.addVertex(pkg); err != nil && !errors.Is(err, graph.ErrVertexAlreadyExists) {
				return nil, fmt.Errorf("unable to add vertex for %s dependency %s: %w", c, dep, err)
			}
			target := packageHash(pkg)
			if isCycle, err := graph.CreatesCycle(g.Graph, packageHash(c), target); err != nil || isCycle {
				pkg = nil
				// we only take the first cycleTarget we find, as we prefer the highest one
				if cycleTarget == "" {
					cycleTarget = target
				}
				continue
			}
			err := g.Graph.AddEdge(packageHash(c), target, graph.EdgeAttribute("target-origin", dep))
			switch {
			case err == nil || errors.Is(err, graph.ErrEdgeAlreadyExists):
				// no error, so we can keep the vertex and we have our match
				return nil, nil
			default:
				return nil, fmt.Errorf("%s: add edge dependency %s error: %w", c, dep, err)
			}
		}
		// did we find a valid dep?
		if pkg == nil {
			if cycleTarget != "" {
				return &cycle{src: packageHash(c), target: cycleTarget}, nil
			}
			if !g.opts.allowUnresolved {
				return nil, fmt.Errorf("%s: unfulfilled dependency %s", c, dep)
			}
			if err := g.addDanglingPackage(dep, c); err != nil {
				return nil, fmt.Errorf("%s: unable to add dangling package %s: %w", c, dep, err)
			}
		}
	}
	return nil, nil
}

// resolveCycle resolves a cycle by trying to reverse the order.
// It discovers what the current dependency is that is causing the potential loop,
// removes the last edge in that cycle, and regenerates that dependency without the previous target.
func (g *Graph) resolveCycle(c *cycle, dep string, resolver *apko.PkgResolver, localRepoSource string) error {
	sp, err := graph.ShortestPath(g.Graph, c.target, c.src)
	if err != nil {
		return fmt.Errorf("unable to find shortest path: %w", err)
	}
	if len(sp) < 2 {
		return fmt.Errorf("there is no path from %s to %s", c.target, c.src)
	}
	// the last edge in the cycle is the one that caused the cycle
	removeSrc, removeTarget := sp[len(sp)-2], sp[len(sp)-1]

	edge, err := g.Graph.Edge(removeSrc, removeTarget)
	if err != nil {
		return fmt.Errorf("unable to find last edge %s -> %s: %w", removeSrc, removeTarget, err)
	}
	if edge.Properties.Attributes == nil {
		return fmt.Errorf("original edge %s -> %s has no attributes", removeSrc, removeTarget)
	}
	origDep := edge.Properties.Attributes["target-origin"]
	// try to reverse the direction of the edge
	if err := g.Graph.RemoveEdge(removeSrc, removeTarget); err != nil {
		return fmt.Errorf("unable to remove original edge %s -> %s: %w", removeSrc, removeTarget, err)
	}
	// add in our new edge
	if err := g.Graph.AddEdge(c.src, c.target, graph.EdgeAttribute("target-origin", dep)); err != nil {
		return fmt.Errorf("unable to add replacement edge %s -> %s: %w", c.src, c.target, err)
	}
	// now we need to re-add the edge that was removed, but with a different target
	config, err := g.Graph.Vertex(removeSrc)
	if err != nil {
		return fmt.Errorf("unable to find original vertex %s: %w", removeSrc, err)
	}
	cycle, err := g.addAppropriatePackage(resolver, config, origDep, localRepoSource)
	if err != nil {
		return fmt.Errorf("unable to re-add original edge %s -> %s: %w", removeSrc, origDep, err)
	}
	if cycle != nil {
		return fmt.Errorf("unable re-add original edge with new dep still causes cycle %s -> %s: %w", removeSrc, dep, err)
	}
	return nil
}

// addVertex adds a vertex to the internal graph, while also tracking its hash by name
func (g *Graph) addVertex(pkg Package) error {
	if err := g.Graph.AddVertex(pkg); err != nil {
		return err
	}
	g.byName[pkg.Name()] = append(g.byName[pkg.Name()], packageHash(pkg))
	return nil
}

func (g *Graph) addDanglingPackage(name string, parent Package) error {
	pkg := danglingPackage{name}
	if err := g.addVertex(pkg); err != nil && !errors.Is(err, graph.ErrVertexAlreadyExists) {
		return err
	}
	if err := g.Graph.AddEdge(packageHash(parent), packageHash(pkg)); err != nil && !errors.Is(err, graph.ErrEdgeAlreadyExists) {
		return err
	}
	return nil
}

// Sorted returns a list of all package names in the Graph, sorted in topological
// order, meaning that packages earlier in the list depend on packages later in
// the list.
func (g Graph) Sorted() ([]Package, error) {
	nodes, err := graph.TopologicalSort(g.Graph)
	if err != nil {
		return nil, err
	}
	pkgs := make([]Package, len(nodes))
	for i, n := range nodes {
		pkgs[i], err = g.Graph.Vertex(n)
		if err != nil {
			return nil, err
		}
	}
	return pkgs, nil
}

// ReverseSorted returns a list of all package names in the Graph, sorted in reverse
// topological order, meaning that packages later in the list depend on packages earlier
// in the list.
func (g Graph) ReverseSorted() ([]Package, error) {
	pkgs, err := g.Sorted()
	if err != nil {
		return nil, err
	}
	for i, j := 0, len(pkgs)-1; i < j; i, j = i+1, j-1 {
		pkgs[i], pkgs[j] = pkgs[j], pkgs[i]
	}
	return pkgs, nil
}

// SubgraphWithRoots returns a new Graph that's a subgraph of g, where the set of
// the new Graph's roots will be identical to or a subset of the given set of
// roots.
//
// In other words, the new subgraph will contain all dependencies (transitively)
// of all packages whose names were given as the `roots` argument.
func (g Graph) SubgraphWithRoots(roots []string) (*Graph, error) {
	// subgraph needs to create a new graph, but it also has a subset of Packages
	subPkgs, err := g.packages.Sub(roots...)
	if err != nil {
		return nil, err
	}
	return NewGraph(subPkgs)
}

// SubgraphWithLeaves returns a new Graph that's a subgraph of g, where the set of
// the new Graph's leaves will be identical to or a subset of the given set of
// leaves.
//
// In other words, the new subgraph will contain all packages (transitively) that
// are dependent on the packages whose names were given as the `leaves` argument.
func (g Graph) SubgraphWithLeaves(leaves []string) (*Graph, error) {
	subgraph := &Graph{
		Graph:  newGraph(),
		opts:   g.opts,
		byName: map[string][]string{},
	}
	var names []string

	predecessorMap, err := g.Graph.PredecessorMap()
	if err != nil {
		return nil, err
	}

	var walk func(key string) error // Go can be so awkward sometimes!
	walk = func(key string) error {
		c := g.packages.ConfigByKey(key)
		if c == nil {
			return fmt.Errorf("unable to find package %q", key)
		}
		if err := subgraph.addVertex(c); err != nil && !errors.Is(err, graph.ErrVertexAlreadyExists) {
			return err
		}
		names = append(names, key)

		for dependent := range predecessorMap[key] {
			c := g.packages.ConfigByKey(dependent)
			if c == nil {
				return fmt.Errorf("unable to find package %q", dependent)
			}
			if err := subgraph.addVertex(c); err != nil && !errors.Is(err, graph.ErrVertexAlreadyExists) {
				return err
			}
			if err := subgraph.Graph.AddEdge(dependent, key); err != nil && !errors.Is(err, graph.ErrEdgeAlreadyExists) {
				return err
			}

			if err := walk(dependent); err != nil {
				return err
			}
		}
		return nil
	}

	for _, leaf := range leaves {
		if err := walk(leaf); err != nil {
			return nil, err
		}
	}

	subPkgs, err := g.packages.Sub(names...)
	if err != nil {
		return nil, err
	}
	subgraph.packages = subPkgs
	return subgraph, nil
}

// Filter is a function that takes a Package and returns true if the Package
// should be included in the filtered Graph, or false if it should be excluded.
type Filter func(Package) bool

// FilterSources returns a Filter that returns true if the Package's source
// matches one of the provided sources, or false otherwise
func FilterSources(source ...string) Filter {
	return func(p Package) bool {
		src := p.Source()
		for _, s := range source {
			if src == s {
				return true
			}
		}
		return false
	}
}

// FilterNotSources returns a Filter that returns false if the Package's source
// matches one of the provided sources, or true otherwise
func FilterNotSources(source ...string) Filter {
	return func(p Package) bool {
		src := p.Source()
		for _, s := range source {
			if src == s {
				return false
			}
		}
		return true
	}
}

// FilterLocal returns a Filter that returns true if the Package's source
// matches the local source, or false otherwise.
func FilterLocal() Filter {
	return FilterSources(Local)
}

// FilterNotLocal returns a Filter that returns true if the Package's source
// matches the local source, or false otherwise.
func FilterNotLocal() Filter {
	return FilterNotSources(Local)
}

// Filter returns a new Graph that's a subgraph of g, where the set of nodes
// in the new graph are filtered by the provided parameters.
// Must provide a func to which each Vertex of type Package is processed, and should return
// true to keep the Vertex and all references to it, or false to remove the Vertex
// and all references to it.
// Some convenience functions are provided for common filtering needs.
func (g Graph) Filter(filter Filter) (*Graph, error) {
	subgraph := &Graph{
		Graph:    newGraph(),
		packages: g.packages,
		opts:     g.opts,
		byName:   map[string][]string{},
	}
	adjacencyMap, err := g.Graph.AdjacencyMap()
	if err != nil {
		return nil, err
	}

	// do this in 2 passes
	// first pass, add all vertices that pass the filter
	// second pass, add all edges whose source and dest are in the new graph
	for node := range adjacencyMap {
		vertex, err := g.Graph.Vertex(node)
		if err != nil {
			return nil, err
		}
		if !filter(vertex) {
			continue
		}
		if err := subgraph.addVertex(vertex); err != nil && !errors.Is(err, graph.ErrVertexAlreadyExists) {
			return nil, err
		}
	}

	for node, deps := range adjacencyMap {
		if _, err := subgraph.Graph.Vertex(node); err != nil {
			continue
		}
		for dep, edge := range deps {
			if _, err := subgraph.Graph.Vertex(dep); err != nil {
				continue
			}
			// both the node and the dependency are in the new graph, so keep the edge
			if err := subgraph.Graph.AddEdge(edge.Source, edge.Target); err != nil && !errors.Is(err, graph.ErrEdgeAlreadyExists) {
				return nil, err
			}
		}
	}
	return subgraph, nil
}

// DependenciesOf returns a slice of the names of the given package's dependencies, sorted alphabetically.
func (g Graph) DependenciesOf(node string) []string {
	adjacencyMap, err := g.Graph.AdjacencyMap()
	if err != nil {
		return nil
	}

	var dependencies []string

	if deps, ok := adjacencyMap[node]; ok {
		for dep := range deps {
			dependencies = append(dependencies, dep)
		}

		// sort for deterministic output
		sort.Strings(dependencies)
		return dependencies
	}

	return nil
}

// Packages returns a slice of the names of all origin packages, sorted alphabetically.
func (g Graph) Packages() []string {
	return g.packages.PackageNames()
}

// Nodes returns a slice of all of the nodes in the graph, sorted alphabetically.
// Unlike Packages, this includes subpackages, provides, etc.
func (g Graph) Nodes() (nodes []string, err error) {
	m, err := g.Graph.AdjacencyMap()
	if err != nil {
		return nil, err
	}
	for node := range m {
		nodes = append(nodes, node)
	}
	sort.Strings(nodes)
	return
}

// NodesByName returns a slice of all of the nodes in the graph for which
// the Vertex's Name() matches the provided name. The sorting order is not guaranteed.
func (g Graph) NodesByName(name string) (pkgs []Package, err error) {
	for _, node := range g.byName[name] {
		pkg, err := g.Graph.Vertex(node)
		if err != nil {
			return nil, err
		}
		pkgs = append(pkgs, pkg)
	}
	return
}

func getKeyMaterial(key string) ([]byte, error) {
	var (
		b     []byte
		asURI uri.URI
		err   error
	)
	if strings.HasPrefix(key, "https://") {
		asURI, err = uri.Parse(key)
		if err != nil {
			return nil, fmt.Errorf("failed to parse key %s as URI: %w", key, err)
		}
	} else {
		asURI = uri.New(key)
	}
	asURL, err := url.Parse(string(asURI))
	if err != nil {
		return nil, fmt.Errorf("failed to parse key %s as URI: %w", key, err)
	}

	switch asURL.Scheme {
	case "file":
		b, err = os.ReadFile(key)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				return nil, fmt.Errorf("failed to read repository %s: %w", key, err)
			}
			return nil, nil
		}
	case "https":
		client := &http.Client{}
		res, err := client.Get(asURL.String())
		if err != nil {
			return nil, fmt.Errorf("unable to get key at %s: %w", key, err)
		}
		defer res.Body.Close()
		buf := bytes.NewBuffer(nil)
		if _, err := io.Copy(buf, res.Body); err != nil {
			return nil, fmt.Errorf("unable to read key at %s: %w", key, err)
		}
		b = buf.Bytes()
	default:
		return nil, fmt.Errorf("key scheme %s not supported", asURL.Scheme)
	}
	return b, nil
}
