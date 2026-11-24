package forwardpolicy

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestCheckACLDenyOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("/bin/chmod", "+a", "everyone deny execute", path).Run(); err != nil {
		t.Fatal(err)
	}

	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkACL(path, info); err != nil {
		t.Fatalf("deny-only ACL rejected: %v", err)
	}

	link := filepath.Join(dir, "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if err := checkACL(link, info); err != ErrUnavailable {
		t.Fatal("symlink matched the target's identity")
	}
}
