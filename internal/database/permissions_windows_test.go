//go:build windows

package database

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestSecurePathPermissionsUsesProtectedWindowsDACL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data.db")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := securePathPermissions(path, false); err != nil {
		t.Fatal(err)
	}
	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		t.Fatal(err)
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sddl := descriptor.String()
	for _, want := range []string{"D:P", user.User.Sid.String(), ";;;SY)", ";;;BA)"} {
		if !strings.Contains(sddl, want) {
			t.Fatalf("DACL %q does not contain %q", sddl, want)
		}
	}
}
