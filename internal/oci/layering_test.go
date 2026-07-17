package oci

import (
	"reflect"
	"testing"
)

// TestAssignLayers pins the partitioning: sorted, one path per layer up to the
// cap, overflow lumped into the last, and a single layer when the cap is <= 1.
func TestAssignLayers(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		roots     []string
		maxLayers int
		want      [][]string
	}{
		{"cap 1 collapses to one layer", []string{"b", "a", "c"}, 1, [][]string{{"a", "b", "c"}}},
		{"cap 0 collapses to one layer", []string{"b", "a"}, 0, [][]string{{"a", "b"}}},
		{"one per layer under the cap", []string{"c", "a", "b"}, 100, [][]string{{"a"}, {"b"}, {"c"}}},
		{"one per layer exactly at the cap", []string{"c", "a", "b"}, 3, [][]string{{"a"}, {"b"}, {"c"}}},
		{"overflow lumps into the last", []string{"e", "d", "c", "b", "a"}, 3, [][]string{{"a"}, {"b"}, {"c", "d", "e"}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := assignLayers(tt.roots, tt.maxLayers, nil); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("assignLayers(%v, %d) = %v, want %v", tt.roots, tt.maxLayers, got, tt.want)
			}
		})
	}
}

// TestAssignLayersIgnoresInputOrder confirms the partition is a pure function of
// the set of roots -- the property that makes layer digests reproducible.
func TestAssignLayersIgnoresInputOrder(t *testing.T) {
	t.Parallel()

	forward := assignLayers([]string{"a", "b", "c", "d"}, 2, nil)
	reversed := assignLayers([]string{"d", "c", "b", "a"}, 2, nil)

	if !reflect.DeepEqual(forward, reversed) {
		t.Errorf("assignment depends on input order:\n  %v\n  %v", forward, reversed)
	}
}
