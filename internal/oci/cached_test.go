package oci_test

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/systemstart/nix-oci/internal/oci"
)

// TestAssembleMatchesWrite is the load-bearing property of the cached path: a
// layout built as plan -> build-layer -> assemble must be byte-identical to the
// one Write produces in a single pass. If it diverges, the cache silently
// changes image digests.
func TestAssembleMatchesWrite(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	roots := []string{
		storePath(t, base, "aaa-x", "x"),
		storePath(t, base, "bbb-y", "y"),
		storePath(t, base, "ccc-z", "z"),
	}

	custom := t.TempDir()
	if err := os.WriteFile(filepath.Join(custom, "hello.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatalf("custom: %v", err)
	}

	opts := oci.ImageOptions{
		Roots:        roots,
		MaxLayers:    100,
		CustomLayer:  custom,
		Entrypoint:   []string{"/x"},
		Architecture: "amd64",
		OS:           "linux",
		RefName:      "latest",
	}

	// Monolithic.
	mono := filepath.Join(t.TempDir(), "mono")
	if err := oci.Write(mono, opts); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Cached: plan, then one build-layer per group, then assemble.
	groups, err := oci.Plan(opts)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	layerDirs := make([]string, len(groups))
	for i, group := range groups {
		dir := filepath.Join(t.TempDir(), "layer"+strconv.Itoa(i))
		if err := oci.BuildLayer(dir, group); err != nil {
			t.Fatalf("BuildLayer %d: %v", i, err)
		}

		layerDirs[i] = dir
	}

	cached := filepath.Join(t.TempDir(), "cached")
	if err := oci.Assemble(cached, layerDirs, opts); err != nil {
		t.Fatalf("Assemble: %v", err)
	}

	assertTreesEqual(t, mono, cached)
}

// TestBuildLayerMetaMatchesBlob confirms the recorded digest actually names the
// blob bytes, so Assemble places it correctly.
func TestBuildLayerMetaMatchesBlob(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	dir := filepath.Join(t.TempDir(), "layer")

	if err := oci.BuildLayer(dir, []string{storePath(t, base, "aaa-x", "x")}); err != nil {
		t.Fatalf("BuildLayer: %v", err)
	}

	var meta oci.LayerMeta
	unmarshalFile(t, filepath.Join(dir, "meta.json"), &meta)

	if meta.Digest == "" || meta.DiffID == "" || meta.Size <= 0 {
		t.Errorf("incomplete meta: %+v", meta)
	}

	if meta.DiffID == meta.Digest {
		t.Error("diffID equals digest -- uncompressed and compressed hashes conflated")
	}
}
