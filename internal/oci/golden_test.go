package oci_test

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/systemstart/nix-oci/internal/oci"
)

// goldenTree writes the fixture the golden digests are taken over: a few
// entries whose names, contents and modes are all stated here, so the resulting
// layer is a pure function of this file plus the code that serialises it.
//
// Modes are chmod'ed rather than left to os.WriteFile, whose argument is masked
// by the caller's umask -- a 0o022 developer and a 0o077 CI runner would
// otherwise produce different tar headers and different digests.
//
// The bulk entry is deliberately compressible: a layer of unique bytes would
// barely exercise the compressor, and this test exists to notice when the
// compressor's output changes.
func goldenTree(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()

	mkdir := func(rel string, mode os.FileMode) {
		t.Helper()

		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}

		if err := os.Chmod(p, mode); err != nil {
			t.Fatalf("chmod %s: %v", rel, err)
		}
	}

	write := func(rel, content string, mode os.FileMode) {
		t.Helper()

		p := filepath.Join(dir, rel)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}

		if err := os.Chmod(p, mode); err != nil {
			t.Fatalf("chmod %s: %v", rel, err)
		}
	}

	mkdir("etc", 0o755)
	write("etc/passwd", "root:x:0:0:root:/root:/bin/sh\n", 0o644)
	write("etc/group", "root:x:0:\n", 0o644)

	mkdir("bin", 0o755)
	write("bin/run", "#!/bin/sh\nexec /nix/store/aaa-x/bin/x \"$@\"\n", 0o755)

	if err := os.Symlink("/nix/store/aaa-x/bin/x", filepath.Join(dir, "bin", "x")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	mkdir("var", 0o755)
	write("var/log.txt", strings.Repeat("nix-oci deterministic layer\n", 512), 0o644)
	write("var/mixed.bin", mixedData(), 0o644)

	return dir
}

// mixedData returns ~256 KiB whose compressibility varies along its length:
// runs of literal text, runs of pseudo-random bytes, and short repeats.
//
// The variety is the point. A fixture of one repeated line compresses to almost
// nothing on any flate implementation, and produced byte-identical output on
// both Go 1.26.7 and 1.27.0 -- it would have made this test a canary that never
// sings. Real layers are mixed, and the Go 1.27 flate change showed up on every
// one of them (identical diffIDs, every blob ~2% larger).
//
// The generator is a xorshift64 written out here rather than math/rand, whose
// stream is explicitly not guaranteed stable across Go releases -- the fixture
// has to be fixed by this file alone, or the test would fail for the wrong
// reason on a toolchain bump.
func mixedData() string {
	var (
		b     strings.Builder
		state uint64 = 0x9E3779B97F4A7C15
	)

	next := func() uint64 {
		state ^= state << 13
		state ^= state >> 7
		state ^= state << 17

		return state
	}

	for block := 0; block < 64; block++ {
		switch block % 4 {
		case 0:
			b.WriteString(strings.Repeat("deterministic-layer-bytes ", 128))
		case 1:
			for i := 0; i < 4096; i++ {
				b.WriteByte(byte(next() & 0xFF))
			}
		case 2:
			for i := 0; i < 512; i++ {
				fmt.Fprintf(&b, "%08x %04d\n", uint32(next()&0xFFFFFFFF), i)
			}
		case 3:
			for i := 0; i < 256; i++ {
				b.WriteString(strings.Repeat(string(rune('a'+i%26)), 16))
			}
		}
	}

	return b.String()
}

// TestGoldenLayerDigest pins the exact bytes nix-oci emits for a fixed tree.
//
// Every other test here checks determinism (the same input twice) or structure,
// and CI's cross-machine job compares two builds of the *same* commit -- so a
// toolchain change that alters compress/flate's output moves every published
// image digest while the whole suite stays green. This test is the one thing
// that goes red.
//
// The two constants separate the causes:
//
//   - DiffID moved: the tar bytes changed -- entry order, headers, modes,
//     mtimes, ownership. Look at layer.go, not at the toolchain.
//   - DiffID held and Digest moved: the tar is identical and the compressor
//     produced different bytes. That is the Go release changing under you.
//
// Neither is a bug on its own, and neither is cosmetic: both mean every image
// nix-oci writes gets a new digest, so consumers pinning by digest must re-pull
// and it belongs in the release notes. Update the constant in the same commit
// that causes the move, and say why there.
func TestGoldenLayerDigest(t *testing.T) {
	t.Parallel()

	const (
		wantDiffID = "sha256:2edac6b1207224bab6dc47d2fd485d41938930a2e6d9ef6b0dfa1263c22eb2db"
		wantDigest = "sha256:1818422f67af545194b16283f5af10578f971705b0b243e6a63644c2c58728c8"
	)

	var buf bytes.Buffer

	res, err := oci.WriteRootedLayer(&buf, goldenTree(t), nil)
	if err != nil {
		t.Fatalf("WriteRootedLayer: %v", err)
	}

	if res.DiffID != wantDiffID {
		t.Errorf("diffID = %s, want %s (the tar bytes changed -- see layer.go)", res.DiffID, wantDiffID)
	}

	if res.Digest != wantDigest {
		t.Errorf("digest = %s, want %s (compressed bytes changed; if diffID held, the Go release moved)",
			res.Digest, wantDigest)
	}

	if int64(buf.Len()) != res.Size {
		t.Errorf("Size = %d, want %d (the byte count must match what was written)", res.Size, buf.Len())
	}
}
