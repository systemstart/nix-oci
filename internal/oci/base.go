package oci

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/opencontainers/go-digest"
)

// baseImage is what a fromImage base contributes: its layer descriptors and
// diffIDs (which our layers sit on top of), its runtime config (which ours
// inherits and overrides), and its manifest digest (recorded as the
// org.opencontainers.image.base.digest annotation).
type baseImage struct {
	layers         []Descriptor
	diffIDs        []digest.Digest
	config         ImageConfig
	manifestDigest digest.Digest
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
		layers:         layers,
		diffIDs:        config.RootFS.DiffIDs,
		config:         config.Config,
		manifestDigest: manifestDesc.Digest,
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
// appended and ExposedPorts/Volumes/Labels are unioned (ours winning per key),
// while scalars and Entrypoint/Cmd replace when set. Fields we never set
// (ArgsEscaped) pass through from the base.
func mergeConfig(base, over ImageConfig) ImageConfig {
	merged := base
	merged.User = pick(over.User, base.User)
	merged.StopSignal = pick(over.StopSignal, base.StopSignal)
	merged.WorkingDir = pick(over.WorkingDir, base.WorkingDir)
	merged.Entrypoint = pickList(over.Entrypoint, base.Entrypoint)
	merged.Cmd = pickList(over.Cmd, base.Cmd)
	merged.Env = append(append([]string{}, base.Env...), over.Env...)
	merged.ExposedPorts = unionSet(base.ExposedPorts, over.ExposedPorts)
	merged.Volumes = unionSet(base.Volumes, over.Volumes)
	merged.Labels = mergeMap(base.Labels, over.Labels)

	return merged
}

// stringSet turns a list into the map[string]struct{} the OCI config uses for
// ExposedPorts and Volumes, or nil when empty.
func stringSet(items []string) map[string]struct{} {
	if len(items) == 0 {
		return nil
	}

	set := make(map[string]struct{}, len(items))
	for _, item := range items {
		set[item] = struct{}{}
	}

	return set
}

// imageAnnotations collects the manifest annotations: the caller's own, plus
// org.opencontainers.image.base.digest when built on a base.
func imageAnnotations(opts ImageOptions, base *baseImage) map[string]string {
	annotations := make(map[string]string, len(opts.Annotations)+1)
	for k, v := range opts.Annotations {
		annotations[k] = v
	}

	if base != nil && base.manifestDigest != "" {
		annotations["org.opencontainers.image.base.digest"] = base.manifestDigest.String()
	}

	if len(annotations) == 0 {
		return nil
	}

	return annotations
}

func pick(over, base string) string {
	if over != "" {
		return over
	}

	return base
}

func pickList(over, base []string) []string {
	if len(over) > 0 {
		return over
	}

	return base
}

func unionSet(base, over map[string]struct{}) map[string]struct{} {
	if len(base) == 0 && len(over) == 0 {
		return nil
	}

	merged := make(map[string]struct{}, len(base)+len(over))
	for k := range base {
		merged[k] = struct{}{}
	}

	for k := range over {
		merged[k] = struct{}{}
	}

	return merged
}

func mergeMap(base, over map[string]string) map[string]string {
	if len(base) == 0 && len(over) == 0 {
		return nil
	}

	merged := make(map[string]string, len(base)+len(over))
	for k, v := range base {
		merged[k] = v
	}

	for k, v := range over {
		merged[k] = v
	}

	return merged
}

// readBlob reads a JSON blob by digest from a layout directory.
func readBlob(dir string, dgst digest.Digest, v any) error {
	return readJSONFile(filepath.Join(dir, "blobs", "sha256", dgst.Encoded()), v)
}
