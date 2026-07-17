package oci

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/opencontainers/go-digest"
)

// baseImage is what a fromImage base contributes: its layer descriptors and
// diffIDs (which our layers sit on top of) and its runtime config (which ours
// inherits and overrides).
type baseImage struct {
	layers  []Descriptor
	diffIDs []digest.Digest
	config  ImageConfig
}

// readBaseImage loads a base OCI image layout, resolving the manifest for the
// target platform. The base's layer blobs are expected in its blobs/sha256/ and
// are copied into the output by copyBaseLayerBlobs.
func readBaseImage(dir, arch, osName string) (*baseImage, error) {
	manifestDesc, err := resolveBaseManifest(dir, arch, osName)
	if err != nil {
		return nil, err
	}

	var manifest Manifest
	if err := readBlob(dir, manifestDesc.Digest, &manifest); err != nil {
		return nil, fmt.Errorf("base manifest: %w", err)
	}

	var config Config
	if err := readBlob(dir, manifest.Config.Digest, &config); err != nil {
		return nil, fmt.Errorf("base config: %w", err)
	}

	// Normalise Docker layer media types to their OCI equivalents so the output
	// manifest stays all-OCI even when the base is a Docker (or hybrid) image.
	// The blobs are identical tar+gzip; only the media-type string changes.
	layers := make([]Descriptor, len(manifest.Layers))
	for i, layer := range manifest.Layers {
		layer.MediaType = normalizeLayerMediaType(layer.MediaType)
		layers[i] = layer
	}

	return &baseImage{
		layers:  layers,
		diffIDs: config.RootFS.DiffIDs,
		config:  config.Config,
	}, nil
}

// normalizeLayerMediaType maps a Docker layer media type to its OCI equivalent
// (identical bytes), leaving anything else untouched.
func normalizeLayerMediaType(mediaType string) string {
	switch mediaType {
	case "application/vnd.docker.image.rootfs.diff.tar.gzip":
		return MediaTypeLayerGzip
	case "application/vnd.docker.image.rootfs.diff.tar":
		return "application/vnd.oci.image.layer.v1.tar"
	default:
		return mediaType
	}
}

// resolveBaseManifest returns the image manifest descriptor in dir matching the
// target platform, resolving nested image indexes (multi-arch bases).
func resolveBaseManifest(dir, arch, osName string) (Descriptor, error) {
	var index Index
	if err := readJSONFile(filepath.Join(dir, "index.json"), &index); err != nil {
		return Descriptor{}, err
	}

	manifests, err := flattenManifests(dir, index.Manifests, 0)
	if err != nil {
		return Descriptor{}, err
	}

	for _, m := range manifests {
		if m.Platform == nil || (m.Platform.Architecture == arch && m.Platform.OS == osName) {
			return m, nil
		}
	}

	if len(manifests) == 1 {
		return manifests[0], nil
	}

	return Descriptor{}, fmt.Errorf("base image: no manifest for %s/%s (found %d)", osName, arch, len(manifests))
}

// flattenManifests expands image-index entries into the image manifests they
// point at.
func flattenManifests(dir string, descs []Descriptor, depth int) ([]Descriptor, error) {
	if depth > 8 {
		return nil, fmt.Errorf("base image: manifest nesting too deep")
	}

	var out []Descriptor

	for _, d := range descs {
		if !isIndexMediaType(d.MediaType) {
			out = append(out, d)

			continue
		}

		var nested Index
		if err := readBlob(dir, d.Digest, &nested); err != nil {
			return nil, err
		}

		sub, err := flattenManifests(dir, nested.Manifests, depth+1)
		if err != nil {
			return nil, err
		}

		out = append(out, sub...)
	}

	return out, nil
}

func isIndexMediaType(mediaType string) bool {
	return mediaType == MediaTypeIndex ||
		mediaType == "application/vnd.docker.distribution.manifest.list.v2+json"
}

// copyBaseLayerBlobs copies the base's layer blobs into blobDir, skipping any
// already present (content-addressed).
func copyBaseLayerBlobs(baseDir, blobDir string, layers []Descriptor) error {
	src := filepath.Join(baseDir, "blobs", "sha256")

	for _, layer := range layers {
		name := layer.Digest.Encoded()

		dst := filepath.Join(blobDir, name)
		if _, err := os.Stat(dst); err == nil {
			continue
		}

		if err := copyBlobFile(filepath.Join(src, name), dst); err != nil {
			return err
		}
	}

	return nil
}

// mergeConfig layers our overrides onto the base's runtime config: Env is
// appended, Entrypoint/Cmd/WorkingDir replace when set, and everything else the
// base declares (ExposedPorts, User, Labels, ...) is inherited unchanged.
func mergeConfig(base, over ImageConfig) ImageConfig {
	merged := base

	if len(over.Env) > 0 {
		merged.Env = append(append([]string{}, base.Env...), over.Env...)
	}

	if len(over.Entrypoint) > 0 {
		merged.Entrypoint = over.Entrypoint
	}

	if len(over.Cmd) > 0 {
		merged.Cmd = over.Cmd
	}

	if over.WorkingDir != "" {
		merged.WorkingDir = over.WorkingDir
	}

	return merged
}

// readBlob reads a JSON blob by digest from a layout directory.
func readBlob(dir string, dgst digest.Digest, v any) error {
	return readJSONFile(filepath.Join(dir, "blobs", "sha256", dgst.Encoded()), v)
}
