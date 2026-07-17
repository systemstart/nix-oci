package oci

import (
	"archive/tar"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
)

// WriteArchive emits an oci-archive -- a tar of the image layout -- to w. This
// is the single-file, streamable form of the same artifact Write produces as a
// directory.
//
// Memory stays constant in image size: the layout is assembled in a temp dir
// (layer blobs are streamed to temp files, never buffered whole), then tarred to
// w a file at a time. The tar is deterministic and ordered blobs-first,
// index.json-last, so a streaming consumer sees every blob before the index that
// references it.
func WriteArchive(w io.Writer, opts ImageOptions) error {
	tmp, err := os.MkdirTemp("", "nix-oci-archive-")
	if err != nil {
		return fmt.Errorf("create temp layout: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	if err := Write(tmp, opts); err != nil {
		return err
	}

	return tarLayout(w, tmp)
}

// tarLayout writes the layout at dir to w as a deterministic tar: the blobs
// directory and its (sorted) blobs first, then oci-layout, then index.json last.
func tarLayout(w io.Writer, dir string) error {
	tw := tar.NewWriter(w)

	for _, d := range []string{"blobs/", "blobs/sha256/"} {
		if err := writeArchiveDir(tw, d); err != nil {
			return err
		}
	}

	blobSha := filepath.Join(dir, "blobs", "sha256")

	entries, err := os.ReadDir(blobSha)
	if err != nil {
		return fmt.Errorf("read blobs: %w", err)
	}

	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}

	sort.Strings(names)

	for _, name := range names {
		if err := writeArchiveFile(tw, "blobs/sha256/"+name, filepath.Join(blobSha, name)); err != nil {
			return err
		}
	}

	// oci-layout before index.json; index.json is the entry point and goes last.
	if err := writeArchiveFile(tw, ImageLayoutFile, filepath.Join(dir, ImageLayoutFile)); err != nil {
		return err
	}

	if err := writeArchiveFile(tw, "index.json", filepath.Join(dir, "index.json")); err != nil {
		return err
	}

	if err := tw.Close(); err != nil {
		return fmt.Errorf("close archive tar: %w", err)
	}

	return nil
}

func writeArchiveDir(tw *tar.Writer, name string) error {
	if err := tw.WriteHeader(&tar.Header{
		Name:     name,
		Typeflag: tar.TypeDir,
		Mode:     dirMode,
		ModTime:  epochTime,
		Format:   tar.FormatPAX,
	}); err != nil {
		return fmt.Errorf("write archive dir %s: %w", name, err)
	}

	return nil
}

// writeArchiveFile streams src into the tar under name, constant memory.
func writeArchiveFile(tw *tar.Writer, name, src string) error {
	info, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("stat %s: %w", src, err)
	}

	if err := tw.WriteHeader(&tar.Header{
		Name:     name,
		Typeflag: tar.TypeReg,
		Mode:     fileMode,
		Size:     info.Size(),
		ModTime:  epochTime,
		Format:   tar.FormatPAX,
	}); err != nil {
		return fmt.Errorf("write archive header %s: %w", name, err)
	}

	f, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer func() { _ = f.Close() }()

	if _, err := io.Copy(tw, f); err != nil {
		return fmt.Errorf("copy %s into archive: %w", name, err)
	}

	return nil
}
