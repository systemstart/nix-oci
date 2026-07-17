//go:build linux

package oci

import (
	"errors"
	"fmt"
	"io/fs"
	"syscall"
)

// readXattrs returns every extended attribute on path as name -> raw-value.
// Filtering to the reproducible subset is the caller's job (xattrAllowed). A
// filesystem that does not support xattrs is not an error -- it yields nothing.
func readXattrs(path string) (map[string]string, error) {
	size, err := syscall.Listxattr(path, nil)
	if err != nil {
		if errors.Is(err, syscall.ENOTSUP) {
			return nil, nil
		}

		return nil, fmt.Errorf("listxattr: %w", err)
	}

	if size == 0 {
		return nil, nil
	}

	buf := make([]byte, size)

	size, err = syscall.Listxattr(path, buf)
	if err != nil {
		return nil, fmt.Errorf("listxattr: %w", err)
	}

	out := make(map[string]string)

	for _, name := range splitNull(buf[:size]) {
		value, err := getxattr(path, name)
		if err != nil {
			// The attribute can vanish between listing and reading, or be
			// unreadable; skip it rather than fail the whole layer.
			continue
		}

		out[name] = value
	}

	return out, nil
}

func getxattr(path, name string) (string, error) {
	size, err := syscall.Getxattr(path, name, nil)
	if err != nil {
		return "", fmt.Errorf("getxattr %s: %w", name, err)
	}

	buf := make([]byte, size)

	size, err = syscall.Getxattr(path, name, buf)
	if err != nil {
		return "", fmt.Errorf("getxattr %s: %w", name, err)
	}

	return string(buf[:size]), nil
}

// splitNull splits a NUL-separated, NUL-terminated attribute-name list (the
// layout listxattr returns).
func splitNull(b []byte) []string {
	var names []string

	start := 0

	for i, c := range b {
		if c == 0 {
			if i > start {
				names = append(names, string(b[start:i]))
			}

			start = i + 1
		}
	}

	return names
}

// deviceNumbers returns the major/minor of a char/block device from its stat.
func deviceNumbers(info fs.FileInfo) (major, minor int64) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0
	}

	return majorMinor(uint64(st.Rdev))
}

// majorMinor decomposes a Linux dev_t using the glibc encoding (gnu_dev_major /
// gnu_dev_minor). The results are masked to 12/20-bit fields, so the int64
// conversions cannot overflow.
func majorMinor(rdev uint64) (major, minor int64) {
	major = int64((rdev>>8)&0xfff | (rdev>>32)&^uint64(0xfff)) //nolint:gosec // masked, fits int64
	minor = int64(rdev&0xff | (rdev>>12)&^uint64(0xff))        //nolint:gosec // masked, fits int64

	return major, minor
}
