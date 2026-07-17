package oci

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestPlanReadsGraphFile covers the graph-file path: Plan reads the registration
// from disk and ranks by popularity (bbb, depended on by aaa, sorts first).
func TestPlanReadsGraphFile(t *testing.T) {
	t.Parallel()

	reg := filepath.Join(t.TempDir(), "registration")
	if err := os.WriteFile(reg, []byte(sampleRegistration), 0o644); err != nil {
		t.Fatalf("write registration: %v", err)
	}

	groups, err := Plan(ImageOptions{
		Roots:        []string{"/nix/store/aaa", "/nix/store/bbb"},
		MaxLayers:    100,
		ClosureGraph: reg,
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	want := [][]string{{"/nix/store/bbb"}, {"/nix/store/aaa"}}
	if !reflect.DeepEqual(groups, want) {
		t.Errorf("Plan = %v, want %v (popularity puts bbb first)", groups, want)
	}
}

// sampleRegistration is two records in closureInfo's format: path, narHash,
// narSize, deriver (blank), reference count, then the references (self included,
// which the parser drops).
const sampleRegistration = `/nix/store/aaa
sha256:hash1
100

2
/nix/store/aaa
/nix/store/bbb
/nix/store/bbb
sha256:hash2
50

1
/nix/store/bbb
`

func TestParseClosureGraph(t *testing.T) {
	t.Parallel()

	refs, err := parseClosureGraph(strings.NewReader(sampleRegistration))
	if err != nil {
		t.Fatalf("parseClosureGraph: %v", err)
	}

	want := map[string][]string{
		"/nix/store/aaa": {"/nix/store/bbb"}, // self-reference dropped
		"/nix/store/bbb": {},                 // only referenced itself
	}

	if !reflect.DeepEqual(refs, want) {
		t.Errorf("refs = %v, want %v", refs, want)
	}
}

func TestParseClosureGraphTruncated(t *testing.T) {
	t.Parallel()

	// A record that promises two references but supplies none.
	truncated := "/nix/store/aaa\nsha256:h\n1\n\n2\n"
	if _, err := parseClosureGraph(strings.NewReader(truncated)); err == nil {
		t.Error("expected an error on a truncated record, got nil")
	}
}

// TestPopularityIsTransitive checks that popularity counts transitive dependents:
// in a chain a -> b -> c, c is depended on by both a and b.
func TestPopularityIsTransitive(t *testing.T) {
	t.Parallel()

	refs := map[string][]string{
		"a": {"b"},
		"b": {"c"},
		"c": {},
	}

	pop := popularityOf(refs)

	want := map[string]int{"a": 0, "b": 1, "c": 2}
	if !reflect.DeepEqual(pop, want) {
		t.Errorf("popularity = %v, want %v", pop, want)
	}
}

// TestAssignLayersByPopularity confirms overflow keeps the most-depended-on paths
// in dedicated layers and lumps the unpopular tail.
func TestAssignLayersByPopularity(t *testing.T) {
	t.Parallel()

	roots := []string{"a", "b", "c", "d"}
	popularity := map[string]int{"a": 0, "b": 0, "c": 5, "d": 5}

	got := assignLayers(roots, 3, popularity)

	// c and d are most popular (tie broken by name); a and b lump together.
	want := [][]string{{"c"}, {"d"}, {"a", "b"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("assignLayers = %v, want %v", got, want)
	}
}
