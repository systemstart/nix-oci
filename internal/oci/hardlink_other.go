//go:build !unix

package oci

import "io/fs"

// statKey is a no-op on platforms without a syscall.Stat_t (e.g. Windows, wasm).
// Hardlink detection is skipped there and every regular file is emitted with its
// own body -- still correct, just not deduplicated. nix-oci does not target
// these platforms (see .goreleaser.yaml), so this only exists to keep the
// package buildable everywhere.
func statKey(_ fs.FileInfo) (fileKey, uint64, bool) {
	return fileKey{}, 0, false
}
