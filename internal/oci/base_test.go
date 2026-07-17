package oci_test

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/systemstart/nix-oci/internal/oci"
)

func statBlob(dir, dgst string) (os.FileInfo, error) {
	info, err := os.Stat(filepath.Join(dir, "blobs", "sha256", strings.TrimPrefix(dgst, "sha256:")))
	if err != nil {
		return nil, fmt.Errorf("stat blob: %w", err)
	}

	return info, nil
}

// imageConfig reads a layout's config blob (via index -> manifest -> config).
func imageConfig(t *testing.T, dir string) oci.Config {
	t.Helper()

	blobDir := filepath.Join(dir, "blobs", "sha256")

	var index oci.Index
	unmarshalFile(t, filepath.Join(dir, "index.json"), &index)

	var manifest oci.Manifest
	unmarshalBlob(t, blobDir, string(index.Manifests[0].Digest), &manifest)

	var config oci.Config
	unmarshalBlob(t, blobDir, string(manifest.Config.Digest), &config)

	return config
}

// TestFromImage checks the fromImage feature end to end at the layout level: the
// base's layer sits beneath ours, diff_ids concatenate, base layer blobs are
// copied in, and the config is inherited (entrypoint) and overridden (env
// appended).
func TestFromImage(t *testing.T) {
	t.Parallel()

	store := t.TempDir()

	// A base image with an entrypoint and env we expect to inherit.
	baseDir := filepath.Join(t.TempDir(), "base")
	baseErr := oci.Write(baseDir, oci.ImageOptions{
		Roots:        []string{storePath(t, store, "aaa-base", "base")},
		MaxLayers:    100,
		Entrypoint:   []string{"/base-entrypoint"},
		Env:          []string{"BASE=1"},
		Architecture: "amd64",
		OS:           "linux",
		RefName:      "latest",
	})
	if baseErr != nil {
		t.Fatalf("write base: %v", baseErr)
	}

	// A new image on top: adds a layer, appends env, inherits the entrypoint.
	outDir := filepath.Join(t.TempDir(), "out")
	if err := oci.Write(outDir, oci.ImageOptions{
		Roots:        []string{storePath(t, store, "bbb-app", "app")},
		MaxLayers:    100,
		BaseImage:    baseDir,
		Env:          []string{"APP=1"},
		Architecture: "amd64",
		OS:           "linux",
		RefName:      "latest",
	}); err != nil {
		t.Fatalf("write from base: %v", err)
	}

	baseLayers := layerDigests(t, baseDir)
	outLayers := layerDigests(t, outDir)

	if len(outLayers) != 2 {
		t.Fatalf("image has %d layers, want 2 (base + ours)", len(outLayers))
	}

	if outLayers[0] != baseLayers[0] {
		t.Errorf("base layer is not beneath ours: got %s, want %s", outLayers[0], baseLayers[0])
	}

	// The base layer blob was copied into the new layout.
	if _, err := statBlob(outDir, outLayers[0]); err != nil {
		t.Errorf("base layer blob missing from the image: %v", err)
	}

	config := imageConfig(t, outDir)

	if len(config.RootFS.DiffIDs) != 2 {
		t.Errorf("diff_ids = %d, want 2", len(config.RootFS.DiffIDs))
	}

	if got := config.Config.Entrypoint; !reflect.DeepEqual(got, []string{"/base-entrypoint"}) {
		t.Errorf("entrypoint = %v, want the inherited [/base-entrypoint]", got)
	}

	if got := config.Config.Env; !reflect.DeepEqual(got, []string{"BASE=1", "APP=1"}) {
		t.Errorf("env = %v, want base then ours [BASE=1 APP=1]", got)
	}
}
