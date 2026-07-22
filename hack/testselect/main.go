// Command testselect picks the minimal set of Go test packages that can
// observe a change, given a list of changed files.
//
// It works off the import graph rather than file paths. A change to package P
// is observable by P's own tests and by the tests of every package that
// transitively imports P, so those are the packages that need to run. Path
// prefix matching misses the transitive cases and over-selects unrelated
// siblings.
//
// Usage:
//
//	git diff --name-only origin/main... | go run ./hack/testselect
//	go run ./hack/testselect -base origin/main
//
// Output is the list of packages to hand to `go test`, one per line.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// pkg is the subset of `go list -json` output we need.
type pkg struct {
	ImportPath   string
	Dir          string
	Imports      []string
	TestImports  []string
	XTestImports []string
	TestGoFiles  []string
	XTestGoFiles []string
}

func main() {
	base := flag.String("base", "", "git ref to diff against; if empty, changed files are read from stdin")
	pattern := flag.Bool("pattern", false, "print a single space-separated line suitable for `go test`")
	explain := flag.Bool("explain", false, "report selection stats on stderr")
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

	// Map changed files to the packages that contain them.
	dirToPkg := make(map[string]string, len(pkgs))
	for _, p := range pkgs {
		rel, err := filepath.Rel(root, p.Dir)
		if err != nil {
			continue
		}
		dirToPkg[filepath.ToSlash(rel)] = p.ImportPath
	}
	seeds := make(map[string]bool)
	nonGo := 0
	for _, f := range changed {
		if !strings.HasSuffix(f, ".go") {
			nonGo++
			continue
		}
		if ip, ok := dirToPkg[filepath.ToSlash(filepath.Dir(f))]; ok {
			seeds[ip] = true
		}
	}

	selected, affected := selectTests(pkgs, module, seeds)

	if *explain {
		testable := 0
		for _, p := range pkgs {
			if len(p.TestGoFiles)+len(p.XTestGoFiles) > 0 {
				testable++
			}
		}
		fmt.Fprintf(os.Stderr, "changed files:      %d (%d non-Go)\n", len(changed), nonGo)
		fmt.Fprintf(os.Stderr, "changed packages:   %d\n", len(seeds))
		fmt.Fprintf(os.Stderr, "affected packages:  %d\n", affected)
		fmt.Fprintf(os.Stderr, "with tests:         %d of %d testable (%.1f%%)\n",
			len(selected), testable, 100*float64(len(selected))/float64(max(testable, 1)))
	}

	if *pattern {
		fmt.Println(strings.Join(selected, " "))
		return
	}
	for _, s := range selected {
		fmt.Println(s)
	}
}

// selectTests returns the packages whose tests can observe a change to any of
// the seed packages, and the size of the affected set before filtering to
// packages that actually have tests.
//
// A change to P is observable by P's own tests and by the tests of anything
// that transitively imports P, so the answer is the reverse-reachable set of
// the seeds over the module-internal import graph. Test imports count: a
// package whose tests import P observes changes to P even if its non-test code
// does not.
func selectTests(pkgs []pkg, module string, seeds map[string]bool) ([]string, int) {
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
	queue := make([]string, 0, len(seeds))
	for s := range seeds {
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

	selected := make([]string, 0, len(affected))
	for ip := range affected {
		if hasTests[ip] {
			selected = append(selected, ip)
		}
	}
	sort.Strings(selected)
	return selected, len(affected)
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
	cmd := exec.Command("git", "diff", "--name-only", base+"...")
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
	cmd := exec.Command("go", "list", "-json", "./...")
	b, err := cmd.Output()
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

	modBytes, err := exec.Command("go", "list", "-m").Output()
	if err != nil {
		return nil, "", "", fmt.Errorf("go list -m: %w", err)
	}
	module := strings.TrimSpace(string(modBytes))

	rootBytes, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return nil, "", "", fmt.Errorf("git rev-parse: %w", err)
	}
	return pkgs, module, strings.TrimSpace(string(rootBytes)), nil
}

func concat(sets ...[]string) []string {
	var out []string
	for _, s := range sets {
		out = append(out, s...)
	}
	return out
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "testselect:", err)
	os.Exit(1)
}
