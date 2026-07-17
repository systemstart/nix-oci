package oci

import "testing"

// TestXattrAllowed pins the reproducibility policy: file capabilities and the
// user.* namespace are carried; host-dependent labels (SELinux) and ACLs are
// dropped so the layer digest does not vary by build host.
func TestXattrAllowed(t *testing.T) {
	t.Parallel()

	allowed := []string{"security.capability", "user.foo", "user."}
	dropped := []string{
		"security.selinux",        // host-dependent label
		"security.ima",            // not carried
		"system.posix_acl_access", // ACL
		"trusted.something",
		"",
	}

	for _, name := range allowed {
		if !xattrAllowed(name) {
			t.Errorf("xattrAllowed(%q) = false, want true", name)
		}
	}

	for _, name := range dropped {
		if xattrAllowed(name) {
			t.Errorf("xattrAllowed(%q) = true, want false", name)
		}
	}
}
