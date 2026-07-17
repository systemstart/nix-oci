//go:build linux

package oci_test

import (
	"archive/tar"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TestXattrCapturedInLayer sets a user.* xattr on a file and checks it comes out
// the far end of the tar+gzip pipeline as a SCHILY.xattr PAX record -- the same
// path security.capability takes, which we cannot set unprivileged.
func TestXattrCapturedInLayer(t *testing.T) {
	t.Parallel()

	store := filepath.Join(t.TempDir(), "store", "aaa-prog")
	if err := os.MkdirAll(store, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	file := filepath.Join(store, "prog")
	if err := os.WriteFile(file, []byte("binary"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	const value = "hello-xattr"
	if err := syscall.Setxattr(file, "user.demo", []byte(value), 0); err != nil {
		if errors.Is(err, syscall.ENOTSUP) {
			t.Skip("filesystem does not support user xattrs")
		}

		t.Fatalf("setxattr: %v", err)
	}

	var got string

	for _, h := range tarHeaders(t, store) {
		if v, ok := h.PAXRecords["SCHILY.xattr.user.demo"]; ok {
			got = v
		}
	}

	if got != value {
		t.Errorf("SCHILY.xattr.user.demo = %q, want %q", got, value)
	}
}

// TestXattrLayerIsReproducible confirms the xattr path does not perturb the
// digest across builds.
func TestXattrLayerIsReproducible(t *testing.T) {
	t.Parallel()

	store := filepath.Join(t.TempDir(), "store", "aaa-prog")
	if err := os.MkdirAll(store, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	file := filepath.Join(store, "prog")
	if err := os.WriteFile(file, []byte("binary"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := syscall.Setxattr(file, "user.demo", []byte("v"), 0); err != nil {
		if errors.Is(err, syscall.ENOTSUP) {
			t.Skip("filesystem does not support user xattrs")
		}

		t.Fatalf("setxattr: %v", err)
	}

	if first, second := writeLayer(t, store), writeLayer(t, store); first.Digest != second.Digest {
		t.Errorf("xattr layer digest not stable:\n  %s\n  %s", first.Digest, second.Digest)
	}
}

// TestDeviceNodePreservesMajorMinor packs /dev/null (a real char device, so no
// privileged mknod is needed) and checks the tar entry carries its major/minor.
// tar.FileInfoHeader cannot supply those, so this exercises the stat-based fill
// in normalizeHeader.
func TestDeviceNodePreservesMajorMinor(t *testing.T) {
	t.Parallel()

	info, err := os.Stat("/dev/null")
	if err != nil || info.Mode()&os.ModeCharDevice == 0 {
		t.Skip("/dev/null is not a char device here")
	}

	var dev *tar.Header

	for _, h := range tarHeaders(t, "/dev/null") {
		if h.Typeflag == tar.TypeChar {
			dev = h
		}
	}

	if dev == nil {
		t.Fatal("no char-device entry emitted for /dev/null")
	}

	// /dev/null is major 1, minor 3 on Linux.
	if dev.Devmajor != 1 || dev.Devminor != 3 {
		t.Errorf("device numbers = %d/%d, want 1/3", dev.Devmajor, dev.Devminor)
	}
}
