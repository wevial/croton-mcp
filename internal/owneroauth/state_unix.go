//go:build linux || darwin

package owneroauth

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// A trusted owner and non-writable parent (or sticky directory) prevents
// another uid replacing a component after Lstat, including below /tmp.
func secureDir(path string, create bool) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
		return errState
	}

	parent := "/"
	for _, component := range strings.Split(path[1:], "/") {
		parent = filepath.Join(parent, component)
		info, err := os.Lstat(parent)
		if errors.Is(err, os.ErrNotExist) && create {
			if err := os.Mkdir(parent, 0700); err != nil && !errors.Is(err, os.ErrExist) {
				return errState
			}
			info, err = os.Lstat(parent)
		}
		if err != nil || !info.IsDir() {
			return errState
		}

		uid := int(info.Sys().(*syscall.Stat_t).Uid)
		if parent == path {
			if uid != os.Geteuid() || info.Mode().Perm() != 0700 {
				return errState
			}
		} else if (uid != 0 && uid != os.Geteuid()) || (info.Mode().Perm()&0022 != 0 && info.Mode()&os.ModeSticky == 0) {
			return errState
		}
	}
	return nil
}

func openDatabase(path string, flags int, mode os.FileMode) (*os.File, error) {
	f, err := os.OpenFile(path, flags|unix.O_NOFOLLOW|unix.O_NONBLOCK, mode)
	if err != nil {
		return nil, errState
	}

	info, err := f.Stat()
	if err == nil && info.Mode().IsRegular() && info.Mode().Perm() == 0600 {
		stat := info.Sys().(*syscall.Stat_t)
		if int(stat.Uid) == os.Geteuid() && stat.Nlink == 1 {
			return f, nil
		}
	}

	f.Close()
	return nil, errState
}
