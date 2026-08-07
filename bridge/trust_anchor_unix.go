//go:build linux || darwin || freebsd

package bridge

import (
	"os"

	"golang.org/x/sys/unix"
)

const trustAnchorFileSupported = true

func openTrustAnchor(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}

	file := os.NewFile(uintptr(fd), path)
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		if err != nil {
			return nil, err
		}
		return nil, os.ErrInvalid
	}

	return file, nil
}
