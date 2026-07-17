package oci

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/opencontainers/go-digest"
)

const (
	dirMode  = 0o755
	fileMode = 0o644
)

// ImageOptions describes the image to write.
type ImageOptions struct {
	// Roots are the store paths to pack.
	Roots []string

	// MaxLayers caps the number of layers. Each store path gets its own layer
	// (maximising cross-image blob reuse in registries) until the cap is
	// reached, after which the remainder share the final layer. 0 or 1 means a
	// single layer. See assignLayers.
	MaxLayers int

	// CustomLayer, if set, is a directory whose contents become a final layer
	// mapped to the image root (e.g. /etc, /tmp). It sorts after all store-path
	// layers so it can shadow them. Content is root-owned. Empty disables it.
	CustomLayer string

	// ClosureGraph, if set, is the path to closureInfo's registration file. It
	// lets layer assignment rank paths by popularity (how many closure members
	// depend on them) so the most-shared paths keep dedicated layers when the
	// closure overflows MaxLayers. Empty falls back to name-order assignment.
	ClosureGraph string

	// BaseImage, if set, is the path to a base OCI image layout. Its layers sit
	// beneath ours and its config is inherited (and overridden by the fields
	// above) -- the fromImage feature. With a base set, Roots may be empty.
	BaseImage string

	Entrypoint []string
	Cmd        []string
	Env        []string
	WorkingDir string

	// The remaining runtime-config fields, all passed straight into the OCI
	// image config. ExposedPorts/Volumes are lists like "8080/tcp" and "/data".
	User         string
	ExposedPorts []string
	Volumes      []string
	Labels       map[string]string
	StopSignal   string

	// Annotations are org.opencontainers.image.* (and any other) key/values set
	// on the image manifest -- provenance like source, revision, version. When a
	// base image is used, org.opencontainers.image.base.digest is added
	// automatically.
	Annotations map[string]string

	Architecture string
	OS           string

	// RefName becomes org.opencontainers.image.ref.name on the manifest
	// descriptor in index.json -- i.e. the tag skopeo and crane select by.
	RefName string
}

// Write emits a complete OCI image layout into dir.
//
// The order here is dictated by the digest chain and cannot be rearranged: the
// layer must be hashed (twice) before the config can name its diffID, the
// config must be hashed before the manifest can reference it, and the manifest
// must be hashed before index.json can point at it.
// Plan returns the layer partition Write would use -- each entry an ordered
// group of store paths -- without compressing anything. It is the cheap first
// step of the cached build path (plan -> build-layer -> assemble); popularity
// ranking (when a closure graph is supplied) keeps the widely-shared base paths
// in their own layers.
func Plan(opts ImageOptions) ([][]string, error) {
	popularity, err := layerPopularity(opts.ClosureGraph)
	if err != nil {
		return nil, err
	}

	return assignLayers(opts.Roots, opts.MaxLayers, popularity), nil
}

func Write(dir string, opts ImageOptions) error {
	blobDir := filepath.Join(dir, "blobs", "sha256")
	if err := os.MkdirAll(blobDir, dirMode); err != nil {
		return fmt.Errorf("create blob dir: %w", err)
	}

	// A fromImage base contributes its layers (copied in) beneath ours, and its
	// config, which ours inherits and overrides.
	if opts.BaseImage == "" {
		return writeWithLayers(dir, blobDir, opts, nil)
	}

	base, err := readBaseImage(opts.BaseImage, opts.Architecture, opts.OS)
	if err != nil {
		return err
	}

	if err := copyBaseLayerBlobs(opts.BaseImage, blobDir, base.layers); err != nil {
		return err
	}

	return writeWithLayers(dir, blobDir, opts, base)
}

// writeWithLayers compresses the store-path layers, adds the customization
// layer, and writes config/manifest/index -- with the base (if any) beneath.
func writeWithLayers(dir, blobDir string, opts ImageOptions, base *baseImage) error {
	// Each group becomes one layer, in order. Because layer digests feed the
	// config's diff_ids and the manifest's descriptors, every layer must be
	// written before either JSON document can be finalized.
	groups, err := Plan(opts)
	if err != nil {
		return err
	}

	// Compress the store-path layers in parallel: they are independent, and
	// gzip is CPU-bound. Results stay in group order, so the output is identical
	// to a sequential build.
	storeLayers, err := compressLayers(blobDir, groups)
	if err != nil {
		return err
	}

	layers := make([]LayerResult, 0, len(groups)+1)
	layers = append(layers, storeLayers...)

	// The customization layer, if any, goes on top so it shadows the store
	// layers.
	custom, err := writeCustomLayer(blobDir, opts.CustomLayer)
	if err != nil {
		return err
	}

	hasCustom := custom != nil
	if hasCustom {
		layers = append(layers, *custom)
	}

	configDesc, err := writeConfigBlob(blobDir, opts, layers, hasCustom, base)
	if err != nil {
		return err
	}

	manifestDesc, err := writeManifestBlob(blobDir, opts, configDesc, layers, base)
	if err != nil {
		return err
	}

	if err := writeIndex(dir, manifestDesc); err != nil {
		return err
	}

	return writeLayoutMarker(dir)
}

// compressLayers writes each group as a layer blob concurrently, bounded by the
// CPU count (gzip is CPU-bound), and returns the results in group order. Layer
// content is a pure function of its group, and results are placed by index, so
// the output is byte-identical regardless of scheduling.
func compressLayers(blobDir string, groups [][]string) ([]LayerResult, error) {
	results := make([]LayerResult, len(groups))
	errs := make([]error, len(groups))

	limit := runtime.GOMAXPROCS(0)
	if limit < 1 {
		limit = 1
	}

	sem := make(chan struct{}, limit)

	var wg sync.WaitGroup

	for i, group := range groups {
		wg.Add(1)

		go func() {
			defer wg.Done()

			sem <- struct{}{}
			defer func() { <-sem }()

			results[i], errs[i] = writeLayerBlob(blobDir, group)
		}()
	}

	wg.Wait()

	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}

	return results, nil
}

// writeLayerBlob writes a store-path layer as a content-addressed blob.
func writeLayerBlob(blobDir string, roots []string) (LayerResult, error) {
	return writeBlobFromLayer(blobDir, func(w io.Writer) (LayerResult, error) {
		return WriteLayer(w, roots)
	})
}

// writeCustomLayer writes the customization layer (srcDir's contents) as a blob,
// or returns nil if srcDir is unset or empty (no point in an empty layer).
func writeCustomLayer(blobDir, srcDir string) (*LayerResult, error) {
	if srcDir == "" {
		return nil, nil
	}

	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return nil, fmt.Errorf("read custom layer dir: %w", err)
	}

	if len(entries) == 0 {
		return nil, nil
	}

	result, err := writeBlobFromLayer(blobDir, func(w io.Writer) (LayerResult, error) {
		return WriteRootedLayer(w, srcDir)
	})
	if err != nil {
		return nil, err
	}

	return &result, nil
}

// writeBlobFromLayer streams a layer to a temp file, then renames it into place
// under its own digest -- the name is not known until the bytes are written.
func writeBlobFromLayer(blobDir string, produce func(io.Writer) (LayerResult, error)) (LayerResult, error) {
	tmp, err := os.CreateTemp(blobDir, ".layer-*")
	if err != nil {
		return LayerResult{}, fmt.Errorf("create temp layer: %w", err)
	}
	// No-op once the rename below succeeds; cleans up the temp file otherwise.
	defer func() { _ = os.Remove(tmp.Name()) }()

	result, err := produce(tmp)
	if err != nil {
		_ = tmp.Close()

		return LayerResult{}, err
	}

	if err := tmp.Close(); err != nil {
		return LayerResult{}, fmt.Errorf("close temp layer: %w", err)
	}

	if err := os.Chmod(tmp.Name(), fileMode); err != nil {
		return LayerResult{}, fmt.Errorf("chmod layer blob: %w", err)
	}

	if err := os.Rename(tmp.Name(), filepath.Join(blobDir, digest.Digest(result.Digest).Encoded())); err != nil {
		return LayerResult{}, fmt.Errorf("rename layer blob: %w", err)
	}

	return result, nil
}

func writeConfigBlob(blobDir string, opts ImageOptions, layers []LayerResult, hasCustom bool, base *baseImage) (Descriptor, error) {
	// diff_ids and history run parallel to the layers, base first. The spec
	// requires the non-empty history entries to correspond one-to-one and in
	// order with rootfs.diff_ids; we emit exactly one non-empty entry per layer
	// (base layers included, with synthesized history so alignment holds
	// regardless of what the base recorded).
	imageConfig := ImageConfig{
		User:         opts.User,
		ExposedPorts: stringSet(opts.ExposedPorts),
		Env:          opts.Env,
		Entrypoint:   opts.Entrypoint,
		Cmd:          opts.Cmd,
		Volumes:      stringSet(opts.Volumes),
		WorkingDir:   opts.WorkingDir,
		Labels:       opts.Labels,
		StopSignal:   opts.StopSignal,
	}

	var (
		diffIDs []digest.Digest
		history []History
	)

	if base != nil {
		diffIDs = append(diffIDs, base.diffIDs...)
		for range base.diffIDs {
			history = append(history, History{Created: &epochTime, CreatedBy: "nix-oci: base image"})
		}

		imageConfig = mergeConfig(base.config, imageConfig)
	}

	for i, layer := range layers {
		createdBy := "nix-oci: nix store closure"
		if hasCustom && i == len(layers)-1 {
			createdBy = "nix-oci: customization layer"
		}

		diffIDs = append(diffIDs, digest.Digest(layer.DiffID))
		history = append(history, History{Created: &epochTime, CreatedBy: createdBy})
	}

	config := Config{
		Created:  &epochTime,
		Platform: Platform{Architecture: opts.Architecture, OS: opts.OS},
		Config:   imageConfig,
		RootFS: RootFS{
			Type:    "layers",
			DiffIDs: diffIDs,
		},
		History: history,
	}

	return writeJSONBlob(blobDir, MediaTypeConfig, config)
}

func writeManifestBlob(
	blobDir string,
	opts ImageOptions,
	config Descriptor,
	layers []LayerResult,
	base *baseImage,
) (Descriptor, error) {
	var descriptors []Descriptor

	if base != nil {
		descriptors = append(descriptors, base.layers...)
	}

	for _, layer := range layers {
		descriptors = append(descriptors, Descriptor{
			MediaType: MediaTypeLayerGzip,
			Digest:    digest.Digest(layer.Digest),
			Size:      layer.Size,
		})
	}

	manifest := Manifest{
		Versioned:   Versioned{SchemaVersion: 2},
		MediaType:   MediaTypeManifest,
		Config:      config,
		Layers:      descriptors,
		Annotations: imageAnnotations(opts, base),
	}

	desc, err := writeJSONBlob(blobDir, MediaTypeManifest, manifest)
	if err != nil {
		return Descriptor{}, err
	}

	desc.Platform = &Platform{Architecture: opts.Architecture, OS: opts.OS}
	if opts.RefName != "" {
		desc.Annotations = map[string]string{AnnotationRefName: opts.RefName}
	}

	return desc, nil
}

func writeIndex(dir string, manifests ...Descriptor) error {
	index := Index{
		Versioned: Versioned{SchemaVersion: 2},
		MediaType: MediaTypeIndex,
		Manifests: manifests,
	}

	data, err := json.Marshal(index)
	if err != nil {
		return fmt.Errorf("marshal index: %w", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "index.json"), data, fileMode); err != nil {
		return fmt.Errorf("write index.json: %w", err)
	}

	return nil
}

func writeLayoutMarker(dir string) error {
	data, err := json.Marshal(ImageLayout{Version: ImageLayoutVersion})
	if err != nil {
		return fmt.Errorf("marshal oci-layout: %w", err)
	}

	if err := os.WriteFile(filepath.Join(dir, ImageLayoutFile), data, fileMode); err != nil {
		return fmt.Errorf("write oci-layout: %w", err)
	}

	return nil
}

// writeJSONBlob marshals v, stores it under its digest, and describes it.
func writeJSONBlob(blobDir, mediaType string, v any) (Descriptor, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return Descriptor{}, fmt.Errorf("marshal %s: %w", mediaType, err)
	}

	dgst := digest.FromBytes(data)

	if err := os.WriteFile(filepath.Join(blobDir, dgst.Encoded()), data, fileMode); err != nil {
		return Descriptor{}, fmt.Errorf("write %s blob: %w", mediaType, err)
	}

	return Descriptor{
		MediaType: mediaType,
		Digest:    dgst,
		Size:      int64(len(data)),
	}, nil
}
