package forwardpolicy

import (
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Darwin ACL grants can override the POSIX mode. Read lstat64_extended's
// kauth_filesec (sys/kauth.h) without following the final component.
func checkACL(path string, info os.FileInfo) error {
	name, err := syscall.BytePtrFromString(path)
	if err != nil {
		return ErrUnavailable
	}

	var sec struct {
		magic        uint32
		owner, group [16]byte
		count, flags uint32
		entries      [128]struct {
			principal     [16]byte
			flags, rights uint32
		}
	}
	var stat syscall.Stat_t
	size := unsafe.Sizeof(sec)
	_, _, errno := syscall.Syscall6(unix.SYS_LSTAT64_EXTENDED, uintptr(unsafe.Pointer(name)),
		uintptr(unsafe.Pointer(&stat)), uintptr(unsafe.Pointer(&sec)), uintptr(unsafe.Pointer(&size)), 0, 0)
	want := info.Sys().(*syscall.Stat_t)
	if errno != 0 || stat.Dev != want.Dev || stat.Ino != want.Ino {
		return ErrUnavailable
	}

	if size == 0 {
		return nil
	}
	const header = unsafe.Offsetof(sec.entries)
	if sec.magic != 0x012cc16d || size < header {
		return ErrUnavailable
	}
	if sec.count == ^uint32(0) && size == header { // KAUTH_FILESEC_NOACL
		return nil
	}
	if sec.count > uint32(len(sec.entries)) || size != header+uintptr(sec.count)*unsafe.Sizeof(sec.entries[0]) {
		return ErrUnavailable
	}
	for _, entry := range sec.entries[:sec.count] {
		if entry.flags&0xf != 2 { // KAUTH_ACE_KINDMASK, KAUTH_ACE_DENY
			return ErrUnavailable
		}
	}
	return nil
}
