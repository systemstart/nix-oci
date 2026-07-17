package oci_test

import (
	"path/filepath"
	"testing"

	"github.com/systemstart/nix-oci/internal/oci"
)

// imageManifest reads a layout's image manifest (via index -> manifest).
func imageManifest(t *testing.T, dir string) oci.Manifest {
	t.Helper()

	var index oci.Index
	unmarshalFile(t, filepath.Join(dir, "index.json"), &index)

	var manifest oci.Manifest
	unmarshalBlob(t, filepath.Join(dir, "blobs", "sha256"), string(index.Manifests[0].Digest), &manifest)

	return manifest
}

// TestConfigPassthrough checks the new runtime-config options land in the image
// config, and provenance annotations land on the manifest.
func TestConfigPassthrough(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	dir := filepath.Join(t.TempDir(), "img")

	if err := oci.Write(dir, oci.ImageOptions{
		Roots:        []string{storePath(t, base, "aaa-x", "x")},
		MaxLayers:    100,
		User:         "1000:1000",
		ExposedPorts: []string{"8080/tcp"},
		Volumes:      []string{"/data"},
		Labels:       map[string]string{"title": "demo"},
		StopSignal:   "SIGINT",
		Annotations:  map[string]string{"org.opencontainers.image.source": "https://ex/repo"},
		Architecture: "amd64",
		OS:           "linux",
		RefName:      "latest",
	}); err != nil {
		t.Fatalf("Write: %v", err)
	}

	c := imageConfig(t, dir).Config

	if c.User != "1000:1000" {
		t.Errorf("user = %q", c.User)
	}

	if _, ok := c.ExposedPorts["8080/tcp"]; !ok {
		t.Errorf("exposed ports = %v", c.ExposedPorts)
	}

	if _, ok := c.Volumes["/data"]; !ok {
		t.Errorf("volumes = %v", c.Volumes)
	}

	if c.Labels["title"] != "demo" {
		t.Errorf("labels = %v", c.Labels)
	}

	if c.StopSignal != "SIGINT" {
		t.Errorf("stopSignal = %q", c.StopSignal)
	}

	if got := imageManifest(t, dir).Annotations["org.opencontainers.image.source"]; got != "https://ex/repo" {
		t.Errorf("source annotation = %q", got)
	}
}

// TestFromImageMergesConfigAndAnnotates checks a base's ExposedPorts are unioned
// with ours, and the base manifest digest is recorded as the base.digest
// annotation.
func TestFromImageMergesConfigAndAnnotates(t *testing.T) {
	t.Parallel()

	store := t.TempDir()

	baseDir := filepath.Join(t.TempDir(), "base")
	if err := oci.Write(baseDir, oci.ImageOptions{
		Roots:        []string{storePath(t, store, "aaa-base", "b")},
		MaxLayers:    100,
		ExposedPorts: []string{"8080/tcp"},
		Architecture: "amd64",
		OS:           "linux",
		RefName:      "latest",
	}); err != nil {
		t.Fatalf("write base: %v", err)
	}

	outDir := filepath.Join(t.TempDir(), "out")
	if err := oci.Write(outDir, oci.ImageOptions{
		Roots:        []string{storePath(t, store, "bbb-app", "a")},
		MaxLayers:    100,
		BaseImage:    baseDir,
		ExposedPorts: []string{"9090/tcp"},
		Architecture: "amd64",
		OS:           "linux",
		RefName:      "latest",
	}); err != nil {
		t.Fatalf("write from base: %v", err)
	}

	ports := imageConfig(t, outDir).Config.ExposedPorts
	if _, ok := ports["8080/tcp"]; !ok {
		t.Errorf("base port not inherited: %v", ports)
	}

	if _, ok := ports["9090/tcp"]; !ok {
		t.Errorf("our port not added: %v", ports)
	}

	var baseIndex oci.Index
	unmarshalFile(t, filepath.Join(baseDir, "index.json"), &baseIndex)

	got := imageManifest(t, outDir).Annotations["org.opencontainers.image.base.digest"]
	if want := string(baseIndex.Manifests[0].Digest); got != want {
		t.Errorf("base.digest annotation = %q, want %q", got, want)
	}
}
