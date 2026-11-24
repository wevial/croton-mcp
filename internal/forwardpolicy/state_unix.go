//go:build linux || darwin

package forwardpolicy

import (
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

// Check parents first so another UID cannot replace a component, even in a sticky directory.
func checkDirectory(path string, private bool) error {
	if path != "/" {
		if err := checkDirectory(filepath.Dir(path), false); err != nil {
			return err
		}
	}

	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() {
		return ErrUnavailable
	}
	uid := int(info.Sys().(*syscall.Stat_t).Uid)
	if private {
		if uid != os.Geteuid() || info.Mode().Perm() != 0700 {
			return ErrUnavailable
		}
	} else if (uid != 0 && uid != os.Geteuid()) || (info.Mode().Perm()&0022 != 0 && info.Mode()&os.ModeSticky == 0) {
		return ErrUnavailable
	}
	return checkACL(path, info)
}

func openFile(path string, create bool) (*os.File, error) {
	flags := os.O_RDWR | unix.O_NOFOLLOW | unix.O_NONBLOCK
	if create {
		flags |= os.O_CREATE | os.O_EXCL
	}

	f, err := os.OpenFile(path, flags, 0600)
	if err != nil {
		return nil, ErrUnavailable
	}

	info, err := f.Stat()
	if err == nil && info.Mode().IsRegular() && info.Mode().Perm() == 0600 && (create || info.Size() > 0) {
		stat := info.Sys().(*syscall.Stat_t)
		if int(stat.Uid) == os.Geteuid() && stat.Nlink == 1 && checkACL(path, info) == nil {
			return f, nil
		}
	}
	_ = f.Close()
	return nil, ErrUnavailable
}
