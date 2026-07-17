package oci

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/opencontainers/go-digest"
)

// Combine merges several single-platform image layouts into one multi-arch
// layout. It unions their blobs -- content-addressed, so a blob shared between
// platforms (or an identical layer) coalesces to one file -- and writes an
// index.json listing every input manifest with its platform descriptor.
//
// This is deliberately cheap: no layer is re-hashed or re-compressed. Each
// per-platform layout is built by an independent derivation (parallelizable on
// remote builders); combining is just a blob union plus an index rewrite.
func Combine(outputDir string, inputDirs []string, ref string) error {
	blobDir := filepath.Join(outputDir, "blobs", "sha256")
	if err := os.MkdirAll(blobDir, dirMode); err != nil {
		return fmt.Errorf("create blob dir: %w", err)
	}

	manifests, err := combineInto(blobDir, inputDirs)
	if err != nil {
		return err
	}

	// The platform manifests go into an image-index blob; the layout's
	// index.json then points at that single blob. Nesting matters: if the
	// platform manifests sat directly in index.json, tools would read them as
	// several separate images rather than one multi-arch image.
	imageIndex := Index{
		Versioned: Versioned{SchemaVersion: 2},
		MediaType: MediaTypeIndex,
		Manifests: manifests,
	}

	indexDesc, err := writeJSONBlob(blobDir, MediaTypeIndex, imageIndex)
	if err != nil {
		return err
	}

	if ref != "" {
		indexDesc.Annotations = map[string]string{AnnotationRefName: ref}
	}

	if err := writeIndex(outputDir, indexDesc); err != nil {
		return err
	}

	return writeLayoutMarker(outputDir)
}

// CombineArchive is Combine streamed as an oci-archive to w.
func CombineArchive(w io.Writer, inputDirs []string, ref string) error {
	tmp, err := os.MkdirTemp("", "nix-oci-combine-")
	if err != nil {
		return fmt.Errorf("create temp layout: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	if err := Combine(tmp, inputDirs, ref); err != nil {
		return err
	}

	return tarLayout(w, tmp)
}

// combineInto copies every input's blobs into blobDir and returns the deduped,
// deterministically-ordered set of platform manifest descriptors.
func combineInto(blobDir string, inputDirs []string) ([]Descriptor, error) {
	var manifests []Descriptor

	seen := make(map[digest.Digest]bool)

	for _, in := range inputDirs {
		var index Index
		if err := readJSONFile(filepath.Join(in, "index.json"), &index); err != nil {
			return nil, err
		}

		for _, m := range index.Manifests {
			if seen[m.Digest] {
				continue
			}

			seen[m.Digest] = true
			// A per-platform manifest is now selected by platform, not by tag, so
			// drop the ref.name annotation the single-image build stamped on it.
			m.Annotations = withoutRefName(m.Annotations)
			manifests = append(manifests, m)
		}

		if err := copyBlobs(filepath.Join(in, "blobs", "sha256"), blobDir); err != nil {
			return nil, err
		}
	}

	// Order is a pure function of the inputs: by platform, then digest.
	sort.Slice(manifests, func(i, j int) bool {
		return manifestKey(manifests[i]) < manifestKey(manifests[j])
	})

	return manifests, nil
}

func manifestKey(d Descriptor) string {
	platform := ""
	if d.Platform != nil {
		platform = d.Platform.OS + "/" + d.Platform.Architecture + "/" + d.Platform.Variant
	}

	return platform + " " + string(d.Digest)
}

func withoutRefName(annotations map[string]string) map[string]string {
	if annotations[AnnotationRefName] == "" {
		return annotations
	}

	out := make(map[string]string, len(annotations))

	for k, v := range annotations {
		if k != AnnotationRefName {
			out[k] = v
		}
	}

	if len(out) == 0 {
		return nil
	}

	return out
}

// copyBlobs copies each blob from src into dst, skipping any already present
// (identical, since the name is the content digest).
func copyBlobs(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return fmt.Errorf("read blobs %s: %w", src, err)
	}

	for _, e := range entries {
		target := filepath.Join(dst, e.Name())
		if _, err := os.Stat(target); err == nil {
			continue // already have this content-addressed blob
		}

		if err := copyBlobFile(filepath.Join(src, e.Name()), target); err != nil {
			return err
		}
	}

	return nil
}

func copyBlobFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer func() { _ = in.Close() }()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fileMode)
	if err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}

	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()

		return fmt.Errorf("copy %s: %w", dst, err)
	}

	if err := out.Close(); err != nil {
		return fmt.Errorf("close %s: %w", dst, err)
	}

	return nil
}

func readJSONFile(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}

	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}

	return nil
}
