package oci_test

import (
	"bytes"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/systemstart/nix-oci/internal/oci"
)

// TestCombineArchive checks the streamed variant produces a non-empty archive.
func TestCombineArchive(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	root := storePath(t, base, "aaa-x", "x")

	amd := writeSingle(t, root, "amd64")
	arm := writeSingle(t, root, "arm64")

	var buf bytes.Buffer
	if err := oci.CombineArchive(&buf, []string{amd, arm}, "latest"); err != nil {
		t.Fatalf("CombineArchive: %v", err)
	}

	if names := archiveNames(t, buf.Bytes()); len(names) == 0 || names[len(names)-1] != "index.json" {
		t.Errorf("combined archive malformed: %v", names)
	}
}

// writeSingle builds a single-platform layout for arch over root, returning its
// dir.
func writeSingle(t *testing.T, root, arch string) string {
	t.Helper()

	dir := filepath.Join(t.TempDir(), arch)
	opts := oci.ImageOptions{
		Roots:        []string{root},
		MaxLayers:    100,
		Architecture: arch,
		OS:           "linux",
		RefName:      "latest",
	}

	if err := oci.Write(dir, opts); err != nil {
		t.Fatalf("Write %s: %v", arch, err)
	}

	return dir
}

// TestCombineMergesPlatforms checks that combining two single-platform layouts
// yields one index with both platform manifests and no leftover ref.name
// annotations (a multi-arch index selects by platform, not tag).
func TestCombineMergesPlatforms(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	root := storePath(t, base, "aaa-x", "x")

	amd := writeSingle(t, root, "amd64")
	arm := writeSingle(t, root, "arm64")

	out := filepath.Join(t.TempDir(), "multi")
	if err := oci.Combine(out, []string{amd, arm}, "latest"); err != nil {
		t.Fatalf("Combine: %v", err)
	}

	blobDir := filepath.Join(out, "blobs", "sha256")

	// The layout's index.json points at a single image-index blob (the
	// multi-arch entry), tagged; the platform manifests live inside that blob.
	var top oci.Index
	unmarshalFile(t, filepath.Join(out, "index.json"), &top)

	if len(top.Manifests) != 1 {
		t.Fatalf("top index has %d entries, want 1 (the image index)", len(top.Manifests))
	}

	if top.Manifests[0].MediaType != oci.MediaTypeIndex {
		t.Errorf("top entry mediaType = %q, want an image index", top.Manifests[0].MediaType)
	}

	if top.Manifests[0].Annotations[oci.AnnotationRefName] != "latest" {
		t.Errorf("image index is not tagged latest: %v", top.Manifests[0].Annotations)
	}

	var index oci.Index
	unmarshalBlob(t, blobDir, string(top.Manifests[0].Digest), &index)

	if len(index.Manifests) != 2 {
		t.Fatalf("image index has %d manifests, want 2", len(index.Manifests))
	}

	arches := checkPlatformManifests(t, blobDir, index.Manifests)

	sort.Strings(arches)

	if arches[0] != "amd64" || arches[1] != "arm64" {
		t.Errorf("platforms = %v, want [amd64 arm64]", arches)
	}
}

// checkPlatformManifests verifies each entry has a platform, no ref.name, and a
// present blob, returning the architectures.
func checkPlatformManifests(t *testing.T, blobDir string, manifests []oci.Descriptor) []string {
	t.Helper()

	var arches []string

	for _, m := range manifests {
		if m.Platform == nil {
			t.Fatalf("manifest %s has no platform", m.Digest)
		}

		arches = append(arches, m.Platform.Architecture)

		if m.Annotations[oci.AnnotationRefName] != "" {
			t.Errorf("manifest %s kept its ref.name annotation", m.Digest)
		}

		if _, err := os.Stat(filepath.Join(blobDir, m.Digest.Encoded())); err != nil {
			t.Errorf("manifest blob %s missing from merged layout: %v", m.Digest, err)
		}
	}

	return arches
}

// TestCombineDedupesSharedBlobs confirms a blob shared between platforms (the
// arch-independent layer) is stored once in the merged layout.
func TestCombineDedupesSharedBlobs(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	root := storePath(t, base, "aaa-x", "x")

	amd := writeSingle(t, root, "amd64")
	arm := writeSingle(t, root, "arm64")

	// The layer carries only content, no arch, so both layouts share it.
	if layerDigests(t, amd)[0] != layerDigests(t, arm)[0] {
		t.Fatal("expected the arch-independent layer to be identical across platforms")
	}

	out := filepath.Join(t.TempDir(), "multi")
	if err := oci.Combine(out, []string{amd, arm}, "latest"); err != nil {
		t.Fatalf("Combine: %v", err)
	}

	// 1 shared layer + 2 configs + 2 manifests + 1 image index = 6 unique blobs.
	entries, err := os.ReadDir(filepath.Join(out, "blobs", "sha256"))
	if err != nil {
		t.Fatalf("read merged blobs: %v", err)
	}

	if len(entries) != 6 {
		t.Errorf("merged layout has %d blobs, want 6 (shared layer deduped)", len(entries))
	}
}
