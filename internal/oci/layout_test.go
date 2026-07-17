package oci_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/systemstart/nix-oci/internal/oci"
)

// TestWriteProducesResolvableLayout drives oci.Write end to end and checks the
// digest chain that holds a layout together: index -> manifest -> {config,
// layer}, with every referenced blob present and content-addressed by its own
// filename. A break anywhere in this chain yields an image tools reject.
func TestWriteProducesResolvableLayout(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "image")
	opts := oci.ImageOptions{
		Roots:        []string{fakeStore(t, "aaa-prog")},
		Entrypoint:   []string{"/bin/prog"},
		Architecture: "arm64",
		OS:           "linux",
		RefName:      "demo:v1",
	}

	if err := oci.Write(dir, opts); err != nil {
		t.Fatalf("Write: %v", err)
	}

	blobDir := filepath.Join(dir, "blobs", "sha256")
	assertContentAddressed(t, blobDir, 3) // layer + config + manifest

	md := assertIndex(t, dir)
	manifest := assertManifest(t, blobDir, string(md.Digest))
	assertConfig(t, blobDir, manifest)
}

// assertContentAddressed checks the blob dir holds wantCount blobs and that each
// file is named by the sha256 of its own contents.
func assertContentAddressed(t *testing.T, blobDir string, wantCount int) {
	t.Helper()

	entries, err := os.ReadDir(blobDir)
	if err != nil {
		t.Fatalf("read blobs: %v", err)
	}

	if len(entries) != wantCount {
		t.Fatalf("expected %d blobs, got %d", wantCount, len(entries))
	}

	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(blobDir, e.Name()))
		if err != nil {
			t.Fatalf("read blob %s: %v", e.Name(), err)
		}

		sum := sha256.Sum256(data)
		if got := hex.EncodeToString(sum[:]); got != e.Name() {
			t.Errorf("blob %s is not addressed by its content (hash %s)", e.Name(), got)
		}
	}
}

// assertIndex checks index.json names exactly one OCI manifest carrying the
// platform and ref annotation Write was given, and returns that descriptor.
func assertIndex(t *testing.T, dir string) oci.Descriptor {
	t.Helper()

	var index oci.Index
	unmarshalFile(t, filepath.Join(dir, "index.json"), &index)

	if index.MediaType != oci.MediaTypeIndex {
		t.Errorf("index mediaType = %q", index.MediaType)
	}

	if len(index.Manifests) != 1 {
		t.Fatalf("index lists %d manifests, want 1", len(index.Manifests))
	}

	md := index.Manifests[0]
	if md.Platform == nil || md.Platform.Architecture != "arm64" || md.Platform.OS != "linux" {
		t.Errorf("manifest platform = %+v, want arm64/linux", md.Platform)
	}

	if md.Annotations[oci.AnnotationRefName] != "demo:v1" {
		t.Errorf("ref annotation = %q, want demo:v1", md.Annotations[oci.AnnotationRefName])
	}

	return md
}

// assertManifest checks the manifest references an OCI config and exactly one
// gzip layer with a recorded size, and returns it.
func assertManifest(t *testing.T, blobDir, digest string) oci.Manifest {
	t.Helper()

	var manifest oci.Manifest
	unmarshalBlob(t, blobDir, digest, &manifest)

	if manifest.Config.MediaType != oci.MediaTypeConfig {
		t.Errorf("config mediaType = %q", manifest.Config.MediaType)
	}

	if len(manifest.Layers) != 1 || manifest.Layers[0].MediaType != oci.MediaTypeLayerGzip {
		t.Fatalf("layers = %+v, want one gzip layer", manifest.Layers)
	}

	if manifest.Layers[0].Size <= 0 {
		t.Errorf("layer descriptor records size %d", manifest.Layers[0].Size)
	}

	return manifest
}

// assertConfig checks the config's rootfs and history: diff_ids are present and
// distinct from the compressed layer digest, and non-empty history entries align
// one-to-one with diff_ids (the spec trap).
func assertConfig(t *testing.T, blobDir string, manifest oci.Manifest) {
	t.Helper()

	var config oci.Config
	unmarshalBlob(t, blobDir, string(manifest.Config.Digest), &config)

	if len(config.RootFS.DiffIDs) != 1 {
		t.Fatalf("rootfs diff_ids = %v, want one", config.RootFS.DiffIDs)
	}

	if config.RootFS.DiffIDs[0] == manifest.Layers[0].Digest {
		t.Error("diff_id equals compressed layer digest -- diffID/digest conflated")
	}

	nonEmpty := 0
	for _, h := range config.History {
		if !h.EmptyLayer {
			nonEmpty++
		}
	}

	if nonEmpty != len(config.RootFS.DiffIDs) {
		t.Errorf("non-empty history entries (%d) must equal diff_ids (%d)", nonEmpty, len(config.RootFS.DiffIDs))
	}
}

// storePath creates a store-path-shaped directory <base>/nix/store/<name> with
// one file and returns its absolute path.
func storePath(t *testing.T, base, name, content string) string {
	t.Helper()

	p := filepath.Join(base, "nix", "store", name)
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if err := os.WriteFile(filepath.Join(p, "file"), []byte(content), 0o444); err != nil {
		t.Fatalf("write: %v", err)
	}

	return p
}

// layerDigests returns the manifest's layer blob digests, in order.
func layerDigests(t *testing.T, layoutDir string) []string {
	t.Helper()

	blobDir := filepath.Join(layoutDir, "blobs", "sha256")

	var index oci.Index
	unmarshalFile(t, filepath.Join(layoutDir, "index.json"), &index)

	var manifest oci.Manifest
	unmarshalBlob(t, blobDir, string(index.Manifests[0].Digest), &manifest)

	digs := make([]string, len(manifest.Layers))
	for i, l := range manifest.Layers {
		digs[i] = string(l.Digest)
	}

	return digs
}

// TestMultiLayerStructure checks that N roots produce N layers with diff_ids and
// history arrays that stay aligned one-to-one (the spec trap that grows teeth
// once there is more than one layer).
func TestMultiLayerStructure(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	roots := []string{
		storePath(t, base, "aaa-x", "x"),
		storePath(t, base, "bbb-y", "y"),
		storePath(t, base, "ccc-z", "z"),
	}

	dir := filepath.Join(t.TempDir(), "image")
	if err := oci.Write(dir, oci.ImageOptions{Roots: roots, MaxLayers: 100, Architecture: "amd64", OS: "linux"}); err != nil {
		t.Fatalf("Write: %v", err)
	}

	blobDir := filepath.Join(dir, "blobs", "sha256")

	var index oci.Index
	unmarshalFile(t, filepath.Join(dir, "index.json"), &index)

	var manifest oci.Manifest
	unmarshalBlob(t, blobDir, string(index.Manifests[0].Digest), &manifest)

	if len(manifest.Layers) != 3 {
		t.Fatalf("manifest has %d layers, want 3", len(manifest.Layers))
	}

	var config oci.Config
	unmarshalBlob(t, blobDir, string(manifest.Config.Digest), &config)

	if len(config.RootFS.DiffIDs) != len(manifest.Layers) {
		t.Errorf("diff_ids (%d) must equal layer count (%d)", len(config.RootFS.DiffIDs), len(manifest.Layers))
	}

	if len(config.History) != len(manifest.Layers) {
		t.Errorf("history entries (%d) must equal layer count (%d)", len(config.History), len(manifest.Layers))
	}
}

// TestSharedLayerDedup is the point of layering: a store path packaged into two
// otherwise-different images produces a byte-identical layer blob, so a registry
// stores it once. Two images share exactly the one path, and its layer digest
// must match in both -- and equal the digest of that path packed alone.
func TestSharedLayerDedup(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	shared := storePath(t, base, "aaa-shared", "shared")
	onlyB := storePath(t, base, "bbb-only", "b")
	onlyC := storePath(t, base, "ccc-only", "c")

	opts := func(roots []string) oci.ImageOptions {
		return oci.ImageOptions{Roots: roots, MaxLayers: 100, Architecture: "amd64", OS: "linux"}
	}

	dir1 := filepath.Join(t.TempDir(), "img1")
	dir2 := filepath.Join(t.TempDir(), "img2")

	if err := oci.Write(dir1, opts([]string{shared, onlyB})); err != nil {
		t.Fatalf("Write img1: %v", err)
	}

	if err := oci.Write(dir2, opts([]string{shared, onlyC})); err != nil {
		t.Fatalf("Write img2: %v", err)
	}

	common := intersect(layerDigests(t, dir1), layerDigests(t, dir2))
	if len(common) != 1 {
		t.Fatalf("images share %d layers, want exactly 1 (the shared path): %v", len(common), common)
	}

	if want := writeLayer(t, shared).Digest; common[0] != want {
		t.Errorf("shared layer digest = %s, want %s (path packed alone)", common[0], want)
	}
}

// TestCustomLayerOnTop checks that a customization directory becomes one extra
// layer above the store layers, aligned in diff_ids/history, and labelled as the
// customization layer in the config history.
func TestCustomLayerOnTop(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	roots := []string{storePath(t, base, "aaa-x", "x")}

	custom := t.TempDir()
	if err := os.MkdirAll(filepath.Join(custom, "etc"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if err := os.WriteFile(filepath.Join(custom, "etc", "passwd"), []byte("root:x:0:0::/:/bin/sh\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	dir := filepath.Join(t.TempDir(), "image")
	opts := oci.ImageOptions{Roots: roots, MaxLayers: 100, CustomLayer: custom, Architecture: "amd64", OS: "linux"}
	if err := oci.Write(dir, opts); err != nil {
		t.Fatalf("Write: %v", err)
	}

	blobDir := filepath.Join(dir, "blobs", "sha256")

	var index oci.Index
	unmarshalFile(t, filepath.Join(dir, "index.json"), &index)

	var manifest oci.Manifest
	unmarshalBlob(t, blobDir, string(index.Manifests[0].Digest), &manifest)

	// one store layer + one custom layer.
	if len(manifest.Layers) != 2 {
		t.Fatalf("manifest has %d layers, want 2 (store + custom)", len(manifest.Layers))
	}

	var config oci.Config
	unmarshalBlob(t, blobDir, string(manifest.Config.Digest), &config)

	if len(config.RootFS.DiffIDs) != 2 || len(config.History) != 2 {
		t.Fatalf("diff_ids=%d history=%d, want 2 each", len(config.RootFS.DiffIDs), len(config.History))
	}

	// The topmost (last) history entry is the customization layer.
	if got := config.History[len(config.History)-1].CreatedBy; got != "nix-oci: customization layer" {
		t.Errorf("top layer CreatedBy = %q, want customization layer", got)
	}
}

// TestEmptyCustomLayerSkipped confirms an empty customization dir adds no layer.
func TestEmptyCustomLayerSkipped(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	roots := []string{storePath(t, base, "aaa-x", "x")}

	dir := filepath.Join(t.TempDir(), "image")
	opts := oci.ImageOptions{Roots: roots, MaxLayers: 100, CustomLayer: t.TempDir(), Architecture: "amd64", OS: "linux"}
	if err := oci.Write(dir, opts); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if got := layerDigests(t, dir); len(got) != 1 {
		t.Errorf("empty custom dir added a layer: %d layers", len(got))
	}
}

func intersect(a, b []string) []string {
	set := make(map[string]bool, len(a))
	for _, s := range a {
		set[s] = true
	}

	var common []string

	for _, s := range b {
		if set[s] {
			common = append(common, s)
		}
	}

	return common
}

// TestWriteIsByteReproducible is the layout-level analogue of the layer
// reproducibility test: two writes of the same options must yield byte-identical
// blobs, index, and marker.
func TestWriteIsByteReproducible(t *testing.T) {
	t.Parallel()

	root := fakeStore(t, "aaa-prog")
	opts := oci.ImageOptions{Roots: []string{root}, Architecture: "amd64", OS: "linux", RefName: "latest"}

	a := filepath.Join(t.TempDir(), "image")
	b := filepath.Join(t.TempDir(), "image")

	if err := oci.Write(a, opts); err != nil {
		t.Fatalf("Write a: %v", err)
	}

	if err := oci.Write(b, opts); err != nil {
		t.Fatalf("Write b: %v", err)
	}

	assertTreesEqual(t, a, b)
}

// TestWriteRejectsBadRoot surfaces the error path: a root that does not exist
// must fail rather than emit a partial layout.
func TestWriteRejectsBadRoot(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "image")
	err := oci.Write(dir, oci.ImageOptions{
		Roots:        []string{filepath.Join(t.TempDir(), "does-not-exist")},
		Architecture: "amd64",
		OS:           "linux",
	})

	if err == nil {
		t.Fatal("expected Write to fail on a nonexistent root")
	}
}

func unmarshalFile(t *testing.T, path string, v any) {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("unmarshal %s: %v", path, err)
	}
}

func unmarshalBlob(t *testing.T, blobDir, digest string, v any) {
	t.Helper()

	hexDigest, ok := strings.CutPrefix(digest, "sha256:")
	if !ok {
		t.Fatalf("digest %q lacks sha256: prefix", digest)
	}

	unmarshalFile(t, filepath.Join(blobDir, hexDigest), v)
}

// assertTreesEqual asserts a and b contain byte-identical sets of files.
func assertTreesEqual(t *testing.T, a, b string) {
	t.Helper()

	filesA := readTree(t, a)
	filesB := readTree(t, b)

	if len(filesA) != len(filesB) {
		t.Fatalf("tree file counts differ: %d vs %d", len(filesA), len(filesB))
	}

	for rel, dataA := range filesA {
		if string(dataA) != string(filesB[rel]) {
			t.Errorf("%s differs between builds", rel)
		}
	}
}

// readTree returns every regular file under root keyed by its path relative to
// root.
func readTree(t *testing.T, root string) map[string][]byte {
	t.Helper()

	files := make(map[string][]byte)

	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err //nolint:wrapcheck // propagating the walk's own error
		}

		if d.IsDir() {
			return nil
		}

		rel, err := filepath.Rel(root, p)
		if err != nil {
			return fmt.Errorf("rel %s: %w", p, err)
		}

		data, err := os.ReadFile(p)
		if err != nil {
			return fmt.Errorf("read %s: %w", p, err)
		}

		files[rel] = data

		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}

	return files
}
