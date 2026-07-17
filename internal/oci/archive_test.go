package oci_test

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/systemstart/nix-oci/internal/oci"
)

func readFile(t *testing.T, path string) []byte {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	return data
}

// archiveNames returns the tar member names in stream order.
func archiveNames(t *testing.T, data []byte) []string {
	t.Helper()

	var names []string

	tr := tar.NewReader(bytes.NewReader(data))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}

		if err != nil {
			t.Fatalf("tar next: %v", err)
		}

		names = append(names, h.Name)
	}

	return names
}

// TestArchiveIsOrderedLayout checks the oci-archive carries the whole layout as
// a tar, blobs first and index.json last (so a streaming reader sees blobs
// before the index that references them).
func TestArchiveIsOrderedLayout(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	opts := oci.ImageOptions{
		Roots:        []string{storePath(t, base, "aaa-x", "x"), storePath(t, base, "bbb-y", "y")},
		MaxLayers:    100,
		Architecture: "amd64",
		OS:           "linux",
	}

	var buf bytes.Buffer
	if err := oci.WriteArchive(&buf, opts); err != nil {
		t.Fatalf("WriteArchive: %v", err)
	}

	names := archiveNames(t, buf.Bytes())

	// blobs/ dirs first, index.json last.
	if len(names) < 4 || names[0] != "blobs/" || names[1] != "blobs/sha256/" {
		t.Fatalf("archive does not start with the blobs dirs: %v", names)
	}

	if last := names[len(names)-1]; last != "index.json" {
		t.Errorf("last member = %q, want index.json", last)
	}

	if indexOf(names, "oci-layout") < 0 {
		t.Error("archive is missing oci-layout")
	}

	assertBlobsPrecedeIndex(t, names)
}

// assertBlobsPrecedeIndex fails if any blob member comes after index.json.
func assertBlobsPrecedeIndex(t *testing.T, names []string) {
	t.Helper()

	indexPos := indexOf(names, "index.json")

	for i, n := range names {
		isBlob := strings.HasPrefix(n, "blobs/sha256/") && n != "blobs/sha256/"
		if isBlob && i > indexPos {
			t.Errorf("blob %q appears after index.json", n)
		}
	}
}

func indexOf(names []string, want string) int {
	for i, n := range names {
		if n == want {
			return i
		}
	}

	return -1
}

// TestArchiveMatchesDirectory confirms the archive is exactly the directory
// layout: every blob and document in the tar is byte-identical to the file Write
// produces, so the two output forms are interchangeable.
func TestArchiveMatchesDirectory(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	opts := oci.ImageOptions{
		Roots:        []string{storePath(t, base, "aaa-x", "x")},
		MaxLayers:    100,
		Architecture: "amd64",
		OS:           "linux",
	}

	dir := filepath.Join(t.TempDir(), "image")
	if err := oci.Write(dir, opts); err != nil {
		t.Fatalf("Write: %v", err)
	}

	var buf bytes.Buffer
	if err := oci.WriteArchive(&buf, opts); err != nil {
		t.Fatalf("WriteArchive: %v", err)
	}

	tr := tar.NewReader(&buf)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}

		if err != nil {
			t.Fatalf("tar next: %v", err)
		}

		if h.Typeflag != tar.TypeReg {
			continue
		}

		fromTar, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("read %s: %v", h.Name, err)
		}

		fromDir := readFile(t, filepath.Join(dir, filepath.FromSlash(h.Name)))
		if !bytes.Equal(fromTar, fromDir) {
			t.Errorf("%s differs between archive and directory", h.Name)
		}
	}
}

// TestArchiveIsReproducible confirms two archives of the same image are
// byte-identical -- the streamed artifact must be reproducible too.
func TestArchiveIsReproducible(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	opts := oci.ImageOptions{
		Roots:        []string{storePath(t, base, "aaa-x", "x"), storePath(t, base, "bbb-y", "y")},
		MaxLayers:    100,
		Architecture: "amd64",
		OS:           "linux",
	}

	var a, b bytes.Buffer
	if err := oci.WriteArchive(&a, opts); err != nil {
		t.Fatalf("WriteArchive a: %v", err)
	}

	if err := oci.WriteArchive(&b, opts); err != nil {
		t.Fatalf("WriteArchive b: %v", err)
	}

	if !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Errorf("archive not reproducible: %d vs %d bytes, differ", a.Len(), b.Len())
	}
}
