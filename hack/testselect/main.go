// Command testselect picks the set of Go test packages that can observe a
// change, given a list of changed files. It is a sound over-approximation: it
// never omits a package whose tests could see the change, and it errs toward
// running more rather than fewer.
//
// It works off the import graph rather than file paths. A change to package P's
// compiled code is observable by P's own tests and by the tests of every
// package that transitively imports P, so those packages are selected. A change
// to only P's test files is observable by P alone, since test files are never
// imported. Path prefix matching misses both the transitive case and this
// distinction.
//
// Files that are not ordinary Go sources are handled explicitly rather than
// ignored:
//
//   - go.mod / go.sum: a dependency change can alter any package, so select
//     everything (fail open).
//   - //go:embed inputs: compiled into the embedding package, treated like a
//     change to that package's code (or its tests, for test-only embeds).
//   - testdata fixtures: read by the owning package's tests, treated as a
//     test-only change to that package.
//   - a changed .go file under no known package (a nested module, a deleted
//     directory): reported on stderr; with -fail-open it forces full selection.
//
// Usage:
//
//	git diff --name-only origin/main... | go run ./hack/testselect
//	go run ./hack/testselect -base origin/main -explain
//	go test $(git diff --name-only origin/main... | go run ./hack/testselect -pattern)
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// pkg is the subset of `go list -json` output we need.
type pkg struct {
	ImportPath      string
	Dir             string
	Imports         []string
	TestImports     []string
	XTestImports    []string
	TestGoFiles     []string
	XTestGoFiles    []string
	EmbedFiles      []string
	TestEmbedFiles  []string
	XTestEmbedFiles []string
}

// fileIndex maps repo-relative paths (slash-separated) to the packages they
// belong to. It is built once from `go list` output.
type fileIndex struct {
	dirToPkg map[string]string   // package directory -> import path
	embed    map[string]embedRef // embedded file -> owning package
}

type embedRef struct {
	importPath string
	test       bool // embedded only into the package's test binary
}

// classification is the result of attributing a set of changed files to
// packages. expand holds packages whose compiled code changed (the change is
// visible to importers); testOnly holds packages where only test inputs
// changed (visible to that package alone).
type classification struct {
	expand    map[string]bool
	testOnly  map[string]bool
	selectAll bool     // a module-wide change (go.mod/go.sum) was seen
	ignored   int      // non-Go files with no package impact
	unmapped  []string // changed .go files that map to no known package
}

func main() {
	base := flag.String("base", "", "git ref to diff against; if empty, changed files are read from stdin")
	pattern := flag.Bool("pattern", false, "print a single space-separated line suitable for `go test`")
	explain := flag.Bool("explain", false, "report selection stats on stderr")
	failOpen := flag.Bool("fail-open", false, "select all packages when a changed .go file maps to no known package")
	flag.Parse()

	changed, err := changedFiles(*base)
	if err != nil {
		fatal(err)
	}
	if len(changed) == 0 {
		return
	}

	pkgs, module, root, err := listPackages()
	if err != nil {
		fatal(err)
	}
	idx := buildIndex(pkgs, root)
	class := classify(changed, idx)

	for _, u := range class.unmapped {
		fmt.Fprintf(os.Stderr, "testselect: changed file maps to no known package: %s\n", u)
	}

	var selected []string
	var affected int
	switch {
	case class.selectAll:
		fmt.Fprintln(os.Stderr, "testselect: module-wide change (go.mod/go.sum), selecting all test packages")
		selected = allTestPackages(pkgs)
		affected = len(pkgs)
	case *failOpen && len(class.unmapped) > 0:
		fmt.Fprintln(os.Stderr, "testselect: unmapped .go files with -fail-open, selecting all test packages")
		selected = allTestPackages(pkgs)
		affected = len(pkgs)
	default:
		selected, affected = selectTests(pkgs, module, class.expand, class.testOnly)
	}

	if *explain {
		testable := len(allTestPackages(pkgs))
		fmt.Fprintf(os.Stderr, "changed files:      %d (%d non-Go, %d unmapped)\n", len(changed), class.ignored, len(class.unmapped))
		fmt.Fprintf(os.Stderr, "changed packages:   %d (%d code, %d test-only)\n",
			len(class.expand)+len(class.testOnly), len(class.expand), len(class.testOnly))
		fmt.Fprintf(os.Stderr, "affected packages:  %d\n", affected)
		fmt.Fprintf(os.Stderr, "with tests:         %d of %d testable (%.1f%%)\n",
			len(selected), testable, 100*float64(len(selected))/float64(max(testable, 1)))
	}

	if *pattern {
		if len(selected) > 0 {
			fmt.Println(strings.Join(selected, " "))
		}
		return
	}
	for _, s := range selected {
		fmt.Println(s)
	}
}

// buildIndex constructs the file-to-package lookup from `go list` output.
func buildIndex(pkgs []pkg, root string) fileIndex {
	idx := fileIndex{
		dirToPkg: make(map[string]string, len(pkgs)),
		embed:    make(map[string]embedRef),
	}
	for _, p := range pkgs {
		rel, err := filepath.Rel(root, p.Dir)
		if err != nil {
			continue
		}
		dir := filepath.ToSlash(rel)
		idx.dirToPkg[dir] = p.ImportPath
		for _, e := range p.EmbedFiles {
			idx.embed[path.Join(dir, e)] = embedRef{p.ImportPath, false}
		}
		for _, e := range concat(p.TestEmbedFiles, p.XTestEmbedFiles) {
			idx.embed[path.Join(dir, e)] = embedRef{p.ImportPath, true}
		}
	}
	return idx
}

// classify attributes each changed file to the package(s) it can affect. It is
// pure over idx, which is the whole point of factoring it out: the mapping
// rules (embeds, testdata, test-only, go.mod) are where the subtle bugs live,
// so they are the part under test.
func classify(changed []string, idx fileIndex) classification {
	c := classification{expand: map[string]bool{}, testOnly: map[string]bool{}}
	for _, f := range changed {
		f = filepath.ToSlash(f)
		base := path.Base(f)

		// A root dependency change can alter any package. Fail open.
		if f == "go.mod" || f == "go.sum" {
			c.selectAll = true
			continue
		}
		// Embedded inputs are compiled into their package (or its tests).
		if ref, ok := idx.embed[f]; ok {
			if ref.test {
				c.testOnly[ref.importPath] = true
			} else {
				c.expand[ref.importPath] = true
			}
			continue
		}
		// testdata fixtures are read only by the owning package's tests.
		if i := strings.Index(f, "/testdata/"); i >= 0 {
			if ip, ok := idx.dirToPkg[f[:i]]; ok {
				c.testOnly[ip] = true
			} else {
				c.unmapped = append(c.unmapped, f)
			}
			continue
		}
		if strings.HasSuffix(f, ".go") {
			ip, ok := idx.dirToPkg[path.Dir(f)]
			if !ok {
				c.unmapped = append(c.unmapped, f)
				continue
			}
			if strings.HasSuffix(base, "_test.go") {
				c.testOnly[ip] = true
			} else {
				c.expand[ip] = true
			}
			continue
		}
		// Anything else (docs, unembedded yaml, CI config) has no package impact.
		c.ignored++
	}
	// A package with any compiled-code change is not test-only.
	for ip := range c.expand {
		delete(c.testOnly, ip)
	}
	return c
}

// selectTests returns the test packages that can observe the classified change,
// and the size of the affected set before filtering to packages with tests.
//
// expand seeds are walked through the reverse import graph (importers observe
// them); testOnly seeds are added alone, since a change to a package's test
// inputs is invisible to importers. Test imports are part of the reverse graph:
// a package whose tests import P observes changes to P.
func selectTests(pkgs []pkg, module string, expand, testOnly map[string]bool) ([]string, int) {
	importers := make(map[string][]string, len(pkgs))
	hasTests := make(map[string]bool, len(pkgs))
	for _, p := range pkgs {
		for _, imp := range concat(p.Imports, p.TestImports, p.XTestImports) {
			if strings.HasPrefix(imp, module) {
				importers[imp] = append(importers[imp], p.ImportPath)
			}
		}
		if len(p.TestGoFiles)+len(p.XTestGoFiles) > 0 {
			hasTests[p.ImportPath] = true
		}
	}

	affected := make(map[string]bool)
	queue := make([]string, 0, len(expand))
	for s := range expand {
		queue = append(queue, s)
	}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if affected[cur] {
			continue
		}
		affected[cur] = true
		queue = append(queue, importers[cur]...)
	}
	// test-only seeds run their own tests but are not expanded through importers
	for s := range testOnly {
		affected[s] = true
	}

	selected := make([]string, 0, len(affected))
	for ip := range affected {
		if hasTests[ip] {
			selected = append(selected, ip)
		}
	}
	sort.Strings(selected)
	return selected, len(affected)
}

func allTestPackages(pkgs []pkg) []string {
	var out []string
	for _, p := range pkgs {
		if len(p.TestGoFiles)+len(p.XTestGoFiles) > 0 {
			out = append(out, p.ImportPath)
		}
	}
	sort.Strings(out)
	return out
}

func changedFiles(base string) ([]string, error) {
	if base == "" {
		var out []string
		sc := bufio.NewScanner(os.Stdin)
		for sc.Scan() {
			if line := strings.TrimSpace(sc.Text()); line != "" {
				out = append(out, line)
			}
		}
		return out, sc.Err()
	}
	// base is the operator's own -base flag, not untrusted input, and passing
	// base+"..." as one argument to a shell-less exec means a leading dash is
	// interpreted as a revision, not a git option.
	cmd := exec.CommandContext(context.Background(), "git", "diff", "--name-only", base+"...") //nolint:gosec // base is an operator-supplied ref
	b, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git diff against %s: %w", base, err)
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out, nil
}

func listPackages() ([]pkg, string, string, error) {
	ctx := context.Background()
	b, err := exec.CommandContext(ctx, "go", "list", "-json", "./...").Output()
	if err != nil {
		return nil, "", "", fmt.Errorf("go list: %w", err)
	}
	var pkgs []pkg
	dec := json.NewDecoder(strings.NewReader(string(b)))
	for dec.More() {
		var p pkg
		if err := dec.Decode(&p); err != nil {
			return nil, "", "", err
		}
		pkgs = append(pkgs, p)
	}

	modBytes, err := exec.CommandContext(ctx, "go", "list", "-m").Output()
	if err != nil {
		return nil, "", "", fmt.Errorf("go list -m: %w", err)
	}
	module := strings.TrimSpace(string(modBytes))

	rootBytes, err := exec.CommandContext(ctx, "git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return nil, "", "", fmt.Errorf("git rev-parse: %w", err)
	}
	return pkgs, module, strings.TrimSpace(string(rootBytes)), nil
}

func concat(sets ...[]string) []string {
	return slices.Concat(sets...)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "testselect:", err)
	os.Exit(1)
}
