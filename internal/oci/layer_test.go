package oci_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/systemstart/nix-oci/internal/oci"
)

// writeLayer packs roots and returns the result, failing the test on error.
func writeLayer(t *testing.T, roots ...string) oci.LayerResult {
	t.Helper()

	result, err := oci.WriteLayer(io.Discard, roots)
	if err != nil {
		t.Fatalf("WriteLayer: %v", err)
	}

	return result
}

// tarHeaders packs roots and returns the uncompressed tar's headers, in stream
// order. It fully drives WriteLayer (tar + gzip), then gunzips and walks the
// result -- so it asserts on the bytes a consumer actually sees.
func tarHeaders(t *testing.T, roots ...string) []*tar.Header {
	t.Helper()

	var buf bytes.Buffer
	if _, err := oci.WriteLayer(&buf, roots); err != nil {
		t.Fatalf("WriteLayer: %v", err)
	}

	return decodeTar(t, &buf)
}

// decodeTar gunzips buf and returns the tar headers in stream order.
func decodeTar(t *testing.T, buf *bytes.Buffer) []*tar.Header {
	t.Helper()

	gz, err := gzip.NewReader(buf)
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}

	var headers []*tar.Header

	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}

		if err != nil {
			t.Fatalf("tar next: %v", err)
		}

		headers = append(headers, h)
	}

	return headers
}

// TestRootedLayerNamesAtRoot checks the customization-layer path: srcDir's
// contents are named relative to srcDir (etc/passwd, not /nix/... and not the
// srcDir prefix), and modes like /tmp's sticky bit survive.
func TestRootedLayerNamesAtRoot(t *testing.T) {
	t.Parallel()

	src := stagingTree(t)

	var buf bytes.Buffer
	if _, err := oci.WriteRootedLayer(&buf, src); err != nil {
		t.Fatalf("WriteRootedLayer: %v", err)
	}

	byName := make(map[string]*tar.Header)
	for _, h := range decodeTar(t, &buf) {
		if strings.HasPrefix(h.Name, "/") || strings.Contains(h.Name, src) {
			t.Errorf("entry %q is not relative to the image root", h.Name)
		}

		byName[h.Name] = h
	}

	for _, want := range []string{"etc/", "etc/passwd", "tmp/"} {
		if byName[want] == nil {
			t.Errorf("missing entry %q; got %v", want, keysOf(byName))
		}
	}

	if h := byName["tmp/"]; h != nil && h.Mode&0o1000 == 0 {
		t.Errorf("tmp/ lost its sticky bit: mode %o", h.Mode)
	}
}

// stagingTree builds a customization-layer source dir with etc/passwd and a
// sticky /tmp, returning its path.
func stagingTree(t *testing.T) string {
	t.Helper()

	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "etc"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if err := os.WriteFile(filepath.Join(src, "etc", "passwd"), []byte("root:x:0:0::/root:/bin/sh\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	tmp := filepath.Join(src, "tmp")
	if err := os.Mkdir(tmp, 0o777); err != nil {
		t.Fatalf("mkdir tmp: %v", err)
	}

	if err := os.Chmod(tmp, os.ModeSticky|0o777); err != nil {
		t.Fatalf("chmod tmp: %v", err)
	}

	return src
}

func keysOf(m map[string]*tar.Header) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}

	sort.Strings(ks)

	return ks
}

// TestHardlinksBecomeLinkEntries checks that a second path pointing at the same
// inode is emitted as a bodyless TypeLink referencing the first, rather than a
// duplicated file body. Without this the store optimiser's hardlinks inflate
// every layer.
func TestHardlinksBecomeLinkEntries(t *testing.T) {
	t.Parallel()

	store := filepath.Join(t.TempDir(), "store", "aaa-prog")
	if err := os.MkdirAll(store, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// "a.txt" sorts before "b.txt", so a.txt is the inode's first appearance and
	// must carry the body; b.txt must become the link.
	orig := filepath.Join(store, "a.txt")
	if err := os.WriteFile(orig, []byte("shared contents"), 0o444); err != nil {
		t.Fatalf("write: %v", err)
	}

	linked := filepath.Join(store, "b.txt")
	if err := os.Link(orig, linked); err != nil {
		t.Fatalf("hardlink: %v", err)
	}

	regName, linkHeader := singleBodyAndLink(t, tarHeaders(t, store))

	if regName == "" {
		t.Fatal("no regular file body was emitted")
	}

	if linkHeader == nil {
		t.Fatal("the hardlinked path was not emitted as a TypeLink")
	}

	if linkHeader.Linkname != regName {
		t.Errorf("link target = %q, want %q (the first-sorted name)", linkHeader.Linkname, regName)
	}

	if linkHeader.Size != 0 {
		t.Errorf("hardlink entry carries a %d-byte body; it must be empty", linkHeader.Size)
	}
}

// singleBodyAndLink scans tar headers, returning the name of the sole file body
// and the link entry, and failing if more than one body carries content.
func singleBodyAndLink(t *testing.T, headers []*tar.Header) (regName string, link *tar.Header) {
	t.Helper()

	for _, h := range headers {
		switch {
		case h.Typeflag == tar.TypeReg && h.Size > 0:
			if regName != "" {
				t.Fatalf("expected exactly one file body, saw a second: %s (after %s)", h.Name, regName)
			}

			regName = h.Name
		case h.Typeflag == tar.TypeLink:
			link = h
		}
	}

	return regName, link
}

// TestHardlinkLayerIsReproducible guards that adding the inode-tracking path did
// not introduce order dependence: the same hardlinked tree must hash the same
// every time.
func TestHardlinkLayerIsReproducible(t *testing.T) {
	t.Parallel()

	store := filepath.Join(t.TempDir(), "store", "aaa-prog")
	if err := os.MkdirAll(store, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	orig := filepath.Join(store, "a.txt")
	if err := os.WriteFile(orig, []byte("shared contents"), 0o444); err != nil {
		t.Fatalf("write: %v", err)
	}

	for _, name := range []string{"b.txt", "c.txt"} {
		if err := os.Link(orig, filepath.Join(store, name)); err != nil {
			t.Fatalf("hardlink %s: %v", name, err)
		}
	}

	if first, second := writeLayer(t, store), writeLayer(t, store); first.Digest != second.Digest {
		t.Errorf("hardlinked layer digest not stable:\n  %s\n  %s", first.Digest, second.Digest)
	}
}

// fakeStore builds a tree shaped like a store path, with deliberately awkward
// metadata: a non-epoch mtime and a mode the layer must preserve.
func fakeStore(t *testing.T, name string) string {
	t.Helper()

	root := filepath.Join(t.TempDir(), "store", name)

	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	bin := filepath.Join(root, "bin", "prog")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho hi\n"), 0o555); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := os.Symlink("prog", filepath.Join(root, "bin", "prog-link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	// A timestamp the layer must normalise away.
	stamp := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	if err := os.Chtimes(bin, stamp, stamp); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	return root
}

// TestLayerIsDeterministic is the core reproducibility property: the same
// closure must produce the same digests every time. If this fails, every digest
// up the chain changes and registry deduplication is defeated.
func TestLayerIsDeterministic(t *testing.T) {
	t.Parallel()

	root := fakeStore(t, "aaa-prog")

	first := writeLayer(t, root)
	second := writeLayer(t, root)

	if first.DiffID != second.DiffID {
		t.Errorf("diffID not stable across builds:\n  %s\n  %s", first.DiffID, second.DiffID)
	}

	if first.Digest != second.Digest {
		t.Errorf("digest not stable across builds:\n  %s\n  %s", first.Digest, second.Digest)
	}
}

// TestLayerIgnoresRootOrder pins the layer as a pure function of the *set* of
// roots. Closure enumeration order is not guaranteed, so if ordering leaked
// into the tar, digests would flap between otherwise identical builds.
func TestLayerIgnoresRootOrder(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	a := filepath.Join(dir, "store", "aaa-one")
	b := filepath.Join(dir, "store", "bbb-two")

	for _, root := range []string{a, b} {
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}

		if err := os.WriteFile(filepath.Join(root, "file"), []byte(root), 0o444); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	forward := writeLayer(t, a, b)
	reversed := writeLayer(t, b, a)

	if forward.Digest != reversed.Digest {
		t.Errorf("digest depends on root order:\n  a,b: %s\n  b,a: %s", forward.Digest, reversed.Digest)
	}
}

// TestDiffIDDiffersFromDigest guards the trap that produces images every
// runtime rejects: diffID names the uncompressed tar, digest names the
// compressed blob. They must never be used interchangeably.
func TestDiffIDDiffersFromDigest(t *testing.T) {
	t.Parallel()

	result := writeLayer(t, fakeStore(t, "aaa-prog"))

	if result.DiffID == result.Digest {
		t.Errorf("diffID and digest are identical (%s) -- one of the two hashers is wired wrong", result.DiffID)
	}

	if result.Size <= 0 {
		t.Errorf("compressed size not recorded: %d", result.Size)
	}
}

// TestMtimeDoesNotAffectDigest is the sharpest reproducibility check: two trees
// with identical content but different mtimes must hash the same, or rebuilding
// a week later produces a different image.
func TestMtimeDoesNotAffectDigest(t *testing.T) {
	t.Parallel()

	// The same root path throughout: only the mtime is allowed to vary. Using a
	// fresh temp dir per build would change the tar entry *names* too, and the
	// resulting digest change would say nothing about mtimes.
	root := filepath.Join(t.TempDir(), "store", "aaa-prog")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, []byte("same content"), 0o444); err != nil {
		t.Fatalf("write: %v", err)
	}

	touch := func(stamp time.Time) string {
		if err := os.Chtimes(file, stamp, stamp); err != nil {
			t.Fatalf("chtimes: %v", err)
		}

		return writeLayer(t, root).DiffID
	}

	old := touch(time.Date(1999, 1, 1, 0, 0, 0, 0, time.UTC))
	recent := touch(time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC))

	if old != recent {
		t.Errorf("mtime leaked into the layer digest:\n  %s\n  %s", old, recent)
	}
}
