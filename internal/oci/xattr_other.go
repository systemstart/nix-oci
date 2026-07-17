//go:build !linux

package oci

import "io/fs"

// readXattrs is a no-op off Linux. security.capability and the SCHILY.xattr
// convention are Linux concerns; darwin closures do not carry Linux file
// capabilities, and we ship no other non-linux targets.
func readXattrs(_ string) (map[string]string, error) {
	return nil, nil
}

// deviceNumbers returns 0/0 off Linux; dev_t decomposition is platform-specific
// and unused on the platforms we target.
func deviceNumbers(_ fs.FileInfo) (major, minor int64) {
	return 0, 0
}
