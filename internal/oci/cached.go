package oci

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/opencontainers/go-digest"
)

// The filenames a BuildLayer output directory holds.
const (
	layerBlobName = "blob"
	layerMetaName = "meta.json"
)

// LayerMeta is the sidecar written next to a cached layer blob: enough for
// Assemble to place it in a layout without recompressing.
type LayerMeta struct {
	DiffID string `json:"diffID"`
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
}

// BuildLayer compresses paths into a single cached layer under outputDir: the
// gzipped tar as `blob`, and its digests and size as `meta.json`.
//
// This is the cacheable unit of the incremental build path. Because a layer is
// a pure function of its store paths (WriteLayer sorts internally), an unchanged
// set of paths yields a byte-identical blob -- so a Nix derivation wrapping
// BuildLayer is reused across builds and never recompresses an unchanged layer.
func BuildLayer(outputDir string, paths []string) error {
	if err := os.MkdirAll(outputDir, dirMode); err != nil {
		return fmt.Errorf("create layer dir: %w", err)
	}

	blob, err := os.Create(filepath.Join(outputDir, layerBlobName))
	if err != nil {
		return fmt.Errorf("create layer blob: %w", err)
	}

	result, err := WriteLayer(blob, paths)
	if err != nil {
		_ = blob.Close()

		return err
	}

	if err := blob.Close(); err != nil {
		return fmt.Errorf("close layer blob: %w", err)
	}

	data, err := json.Marshal(LayerMeta(result))
	if err != nil {
		return fmt.Errorf("marshal layer meta: %w", err)
	}

	if err := os.WriteFile(filepath.Join(outputDir, layerMetaName), data, fileMode); err != nil {
		return fmt.Errorf("write layer meta: %w", err)
	}

	return nil
}

// Assemble builds a complete layout from precomputed layer directories (each a
// BuildLayer output), in order, without recompressing them. This is the finale
// of the cached path -- plan, then one cached build-layer per layer, then this.
// Its output is byte-identical to Write's for the same inputs: the layer blobs
// are the same bytes, and the config/manifest/index are built the same way.
func Assemble(outputDir string, layerDirs []string, opts ImageOptions) error {
	blobDir := filepath.Join(outputDir, "blobs", "sha256")
	if err := os.MkdirAll(blobDir, dirMode); err != nil {
		return fmt.Errorf("create blob dir: %w", err)
	}

	layers, err := loadCachedLayers(blobDir, layerDirs)
	if err != nil {
		return err
	}

	// The customization layer is build-local (not cached) and goes on top.
	custom, err := writeCustomLayer(blobDir, opts.CustomLayer)
	if err != nil {
		return err
	}

	hasCustom := custom != nil
	if hasCustom {
		layers = append(layers, *custom)
	}

	configDesc, err := writeConfigBlob(blobDir, opts, layers, hasCustom, nil)
	if err != nil {
		return err
	}

	manifestDesc, err := writeManifestBlob(blobDir, opts, configDesc, layers, nil)
	if err != nil {
		return err
	}

	if err := writeIndex(outputDir, manifestDesc); err != nil {
		return err
	}

	return writeLayoutMarker(outputDir)
}

// loadCachedLayers reads each layer dir's metadata and copies its blob into
// blobDir (skipping ones already present), returning the layer results in order.
func loadCachedLayers(blobDir string, layerDirs []string) ([]LayerResult, error) {
	layers := make([]LayerResult, 0, len(layerDirs)+1)

	for _, dir := range layerDirs {
		var meta LayerMeta
		if err := readJSONFile(filepath.Join(dir, layerMetaName), &meta); err != nil {
			return nil, err
		}

		dst := filepath.Join(blobDir, digest.Digest(meta.Digest).Encoded())
		if _, err := os.Stat(dst); err != nil {
			if err := copyBlobFile(filepath.Join(dir, layerBlobName), dst); err != nil {
				return nil, err
			}
		}

		layers = append(layers, LayerResult(meta))
	}

	return layers, nil
}
