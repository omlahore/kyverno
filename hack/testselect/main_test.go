package main

import (
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
)

const mod = "example.com/m"

// graph:
//
//	leaf   <- mid <- top        (top and mid import leaf transitively)
//	leaf   <- viatest           (imports leaf only from its tests)
//	lonely                      (imports nothing internal, has tests)
//	notest <- top               (imported, but has no test files)
func fixture() []pkg {
	return []pkg{
		{ImportPath: mod + "/leaf", TestGoFiles: []string{"leaf_test.go"}},
		{ImportPath: mod + "/mid", Imports: []string{mod + "/leaf"}, TestGoFiles: []string{"mid_test.go"}},
		{ImportPath: mod + "/top", Imports: []string{mod + "/mid", mod + "/notest"}, TestGoFiles: []string{"top_test.go"}},
		{ImportPath: mod + "/viatest", TestImports: []string{mod + "/leaf"}, TestGoFiles: []string{"viatest_test.go"}},
		{ImportPath: mod + "/notest", Imports: []string{mod + "/leaf"}},
		{ImportPath: mod + "/lonely", Imports: []string{"k8s.io/apimachinery/pkg/runtime"}, TestGoFiles: []string{"lonely_test.go"}},
	}
}

func set(xs ...string) map[string]bool {
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		m[x] = true
	}
	return m
}

func TestSelectTests(t *testing.T) {
	tests := []struct {
		name     string
		expand   map[string]bool
		testOnly map[string]bool
		want     []string
		affected int
	}{
		{
			name:   "code change reaches transitive and test-only importers",
			expand: set(mod + "/leaf"),
			// notest is affected but contributes no tests to run
			want:     []string{mod + "/leaf", mod + "/mid", mod + "/top", mod + "/viatest"},
			affected: 5,
		},
		{
			name:     "mid change does not reach leaf downwards",
			expand:   set(mod + "/mid"),
			want:     []string{mod + "/mid", mod + "/top"},
			affected: 2,
		},
		{
			name:     "package with no internal importers selects only itself",
			expand:   set(mod + "/lonely"),
			want:     []string{mod + "/lonely"},
			affected: 1,
		},
		{
			name:     "changing an untested package still runs its importers",
			expand:   set(mod + "/notest"),
			want:     []string{mod + "/top"},
			affected: 2,
		},
		{
			name:     "test-only change selects just that package, no importers",
			testOnly: set(mod + "/leaf"),
			want:     []string{mod + "/leaf"},
			affected: 1,
		},
		{
			name:     "nothing changed selects nothing",
			want:     []string{},
			affected: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, affected := selectTests(fixture(), mod, tt.expand, tt.testOnly)
			assert.Equal(t, tt.want, got)
			assert.Equal(t, tt.affected, affected)
		})
	}
}

func TestSelectTestsIgnoresExternalImports(t *testing.T) {
	// an external package must never end up in the reverse graph
	_, affected := selectTests(fixture(), mod, set("k8s.io/apimachinery/pkg/runtime"), nil)
	assert.Equal(t, 1, affected, "external package should reach nothing internal")
}

func TestClassify(t *testing.T) {
	// index modelled on real kyverno cases: an embedding package, a package
	// with a testdata fixture, and a test-embed.
	idx := fileIndex{
		dirToPkg: map[string]string{
			"pkg/engine/resources": mod + "/pkg/engine/resources",
			"pkg/engine/mutate":    mod + "/pkg/engine/mutate",
			"pkg/policy/common":    mod + "/pkg/policy/common",
		},
		embed: map[string]embedRef{
			"pkg/engine/resources/default-config.yaml": {mod + "/pkg/engine/resources", false},
			"pkg/engine/mutate/testfix.yaml":           {mod + "/pkg/engine/mutate", true},
		},
	}

	tests := []struct {
		name          string
		changed       []string
		wantExpand    []string
		wantTestOnly  []string
		wantSelectAll bool
		wantUnmapped  []string
		wantIgnored   int
	}{
		{
			name:       "ordinary code file expands",
			changed:    []string{"pkg/policy/common/validate_pattern.go"},
			wantExpand: []string{mod + "/pkg/policy/common"},
		},
		{
			name:         "test file is test-only",
			changed:      []string{"pkg/policy/common/validate_pattern_test.go"},
			wantTestOnly: []string{mod + "/pkg/policy/common"},
		},
		{
			name:       "code plus test change in one package expands (not test-only)",
			changed:    []string{"pkg/policy/common/validate_pattern.go", "pkg/policy/common/validate_pattern_test.go"},
			wantExpand: []string{mod + "/pkg/policy/common"},
		},
		{
			name:       "embedded input is treated as a code change to its package",
			changed:    []string{"pkg/engine/resources/default-config.yaml"},
			wantExpand: []string{mod + "/pkg/engine/resources"},
		},
		{
			name:         "test-embedded input is test-only",
			changed:      []string{"pkg/engine/mutate/testfix.yaml"},
			wantTestOnly: []string{mod + "/pkg/engine/mutate"},
		},
		{
			name:         "testdata fixture is test-only for its owning package",
			changed:      []string{"pkg/engine/mutate/testdata/endpoints.yaml"},
			wantTestOnly: []string{mod + "/pkg/engine/mutate"},
		},
		{
			name:          "go.mod forces select-all",
			changed:       []string{"go.mod"},
			wantSelectAll: true,
		},
		{
			name:          "go.sum forces select-all",
			changed:       []string{"go.sum"},
			wantSelectAll: true,
		},
		{
			name:         "go file under no known package is unmapped",
			changed:      []string{"hack/controller-gen/main.go"},
			wantUnmapped: []string{"hack/controller-gen/main.go"},
		},
		{
			name:        "unembedded, non-test data file is ignored",
			changed:     []string{"README.md", ".github/workflows/ci.yaml"},
			wantIgnored: 2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := classify(tt.changed, idx)
			assert.Equal(t, sortedKeys(c.expand), sortStrings(tt.wantExpand))
			assert.Equal(t, sortedKeys(c.testOnly), sortStrings(tt.wantTestOnly))
			assert.Equal(t, tt.wantSelectAll, c.selectAll)
			assert.Equal(t, sortStrings(tt.wantUnmapped), sortStrings(c.unmapped))
			assert.Equal(t, tt.wantIgnored, c.ignored)
		})
	}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortStrings(xs []string) []string {
	out := append([]string(nil), xs...)
	sort.Strings(out)
	if out == nil {
		return []string{}
	}
	return out
}
