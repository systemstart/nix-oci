//go:build unix

package oci

import (
	"io/fs"
	"syscall"
)

// statKey extracts a file's (device, inode) identity and its link count from the
// os.FileInfo. Two paths with the same key are the same inode -- i.e.
// hardlinks. ok is false when the platform does not expose a syscall.Stat_t, in
// which case the caller skips hardlink detection and emits a full copy.
func statKey(info fs.FileInfo) (key fileKey, nlink uint64, ok bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fileKey{}, 0, false
	}

	// The field widths differ per OS (e.g. Dev is int32 on darwin, uint64 on
	// linux; Nlink is uint16 vs uint64), so widen everything to uint64.
	return fileKey{dev: uint64(st.Dev), ino: uint64(st.Ino)}, uint64(st.Nlink), true
}
