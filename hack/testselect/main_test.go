package main

import (
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

func TestSelectTests(t *testing.T) {
	tests := []struct {
		name     string
		seeds    []string
		want     []string
		affected int
	}{
		{
			name:  "leaf change reaches transitive importers and test-only importers",
			seeds: []string{mod + "/leaf"},
			// notest is affected but contributes no tests to run
			want:     []string{mod + "/leaf", mod + "/mid", mod + "/top", mod + "/viatest"},
			affected: 5,
		},
		{
			name:     "mid change does not reach leaf downwards",
			seeds:    []string{mod + "/mid"},
			want:     []string{mod + "/mid", mod + "/top"},
			affected: 2,
		},
		{
			name:     "leaf package with no internal importers selects only itself",
			seeds:    []string{mod + "/lonely"},
			want:     []string{mod + "/lonely"},
			affected: 1,
		},
		{
			name:     "changing an untested package still runs its importers",
			seeds:    []string{mod + "/notest"},
			want:     []string{mod + "/top"},
			affected: 2,
		},
		{
			name:     "no changes selects nothing",
			seeds:    nil,
			want:     []string{},
			affected: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			seeds := make(map[string]bool, len(tt.seeds))
			for _, s := range tt.seeds {
				seeds[s] = true
			}
			got, affected := selectTests(fixture(), mod, seeds)
			assert.Equal(t, tt.want, got)
			assert.Equal(t, tt.affected, affected)
		})
	}
}

func TestSelectTestsIgnoresExternalImports(t *testing.T) {
	// a change to an external dependency is not expressible as a seed, but
	// external imports must never end up in the reverse graph
	_, affected := selectTests(fixture(), mod, map[string]bool{"k8s.io/apimachinery/pkg/runtime": true})
	assert.Equal(t, 1, affected, "external package should reach nothing internal")
}
