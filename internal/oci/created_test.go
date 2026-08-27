package oci_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/systemstart/nix-oci/internal/oci"
)

// writeWithCreated builds a one-root image, varying only Created.
//
// root is passed in rather than made here: store paths are absolute and appear
// verbatim as tar entry names, so two builds must share one root or their
// layers differ for reasons having nothing to do with the timestamp.
func writeWithCreated(t *testing.T, root string, created *time.Time) string {
	t.Helper()

	dir := filepath.Join(t.TempDir(), "img")

	if err := oci.Write(dir, oci.ImageOptions{
		Roots:        []string{root},
		MaxLayers:    100,
		Architecture: "amd64",
		OS:           "linux",
		RefName:      "latest",
		Created:      created,
	}); err != nil {
		t.Fatalf("Write: %v", err)
	}

	return dir
}

// TestCreatedDefaultsToEpoch pins the default: without an explicit Created the
// config and its history stay at 1970, which is what keeps the config blob a
// pure function of the content.
func TestCreatedDefaultsToEpoch(t *testing.T) {
	t.Parallel()

	root := storePath(t, t.TempDir(), "aaa-x", "x")
	cfg := imageConfig(t, writeWithCreated(t, root, nil))

	if cfg.Created == nil {
		t.Fatal("created is nil, want the epoch")
	}

	if !cfg.Created.Equal(time.Unix(0, 0)) {
		t.Errorf("created = %v, want 1970-01-01T00:00:00Z", cfg.Created)
	}

	for i, h := range cfg.History {
		if h.Created == nil || !h.Created.Equal(time.Unix(0, 0)) {
			t.Errorf("history[%d].created = %v, want the epoch", i, h.Created)
		}
	}
}

// TestCreatedIsHonoured checks an explicit timestamp reaches both the config
// and every history entry.
func TestCreatedIsHonoured(t *testing.T) {
	t.Parallel()

	want := time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)
	root := storePath(t, t.TempDir(), "aaa-x", "x")
	cfg := imageConfig(t, writeWithCreated(t, root, &want))

	if cfg.Created == nil || !cfg.Created.Equal(want) {
		t.Errorf("created = %v, want %v", cfg.Created, want)
	}

	if len(cfg.History) == 0 {
		t.Fatal("no history entries")
	}

	for i, h := range cfg.History {
		if h.Created == nil || !h.Created.Equal(want) {
			t.Errorf("history[%d].created = %v, want %v", i, h.Created, want)
		}
	}
}

// TestCreatedLeavesLayersAlone is the property that makes this option safe to
// use: changing the timestamp must move the config blob and nothing else. Tar
// entry mtimes stay at the epoch, so layer blobs keep their digests and stay
// shareable between images built with different timestamps.
func TestCreatedLeavesLayersAlone(t *testing.T) {
	t.Parallel()

	stamped := time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)

	root := storePath(t, t.TempDir(), "aaa-x", "x")
	epochDir := writeWithCreated(t, root, nil)
	stampedDir := writeWithCreated(t, root, &stamped)

	epochManifest := imageManifest(t, epochDir)
	stampedManifest := imageManifest(t, stampedDir)

	if len(epochManifest.Layers) != len(stampedManifest.Layers) {
		t.Fatalf("layer count differs: %d vs %d", len(epochManifest.Layers), len(stampedManifest.Layers))
	}

	for i := range epochManifest.Layers {
		if epochManifest.Layers[i].Digest != stampedManifest.Layers[i].Digest {
			t.Errorf("layer[%d] digest moved with created: %s vs %s",
				i, epochManifest.Layers[i].Digest, stampedManifest.Layers[i].Digest)
		}
	}

	// diff_ids name the uncompressed tars; they must be untouched too.
	epochCfg := imageConfig(t, epochDir)
	stampedCfg := imageConfig(t, stampedDir)

	for i := range epochCfg.RootFS.DiffIDs {
		if epochCfg.RootFS.DiffIDs[i] != stampedCfg.RootFS.DiffIDs[i] {
			t.Errorf("diff_id[%d] moved with created", i)
		}
	}

	// The config blob itself must move, or the field did nothing.
	if epochManifest.Config.Digest == stampedManifest.Config.Digest {
		t.Error("config digest unchanged; created had no effect")
	}
}

// TestCreatedIsDeterministic: same timestamp in, same digests out.
func TestCreatedIsDeterministic(t *testing.T) {
	t.Parallel()

	stamp := time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)

	root := storePath(t, t.TempDir(), "aaa-x", "x")
	a := imageManifest(t, writeWithCreated(t, root, &stamp))
	b := imageManifest(t, writeWithCreated(t, root, &stamp))

	if a.Config.Digest != b.Config.Digest {
		t.Errorf("config digest not reproducible: %s vs %s", a.Config.Digest, b.Config.Digest)
	}
}
