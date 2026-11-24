package forwardpolicy

import (
	"os"
	"sync"
	"syscall"
	"unsafe"

	"github.com/ebitengine/purego"
	"golang.org/x/sys/unix"
)

// getattrlist packs the requested common attributes in bit order, followed
// by the variable-length kauth_filesec (sys/attr.h and sys/kauth.h).
type aclAttributes struct {
	length    uint32
	device    int32
	aclOffset int32 // relative to the start of this field
	aclLength uint32
	inode     uint64
	security  struct {
		magic        uint32
		owner, group [16]byte
		count, flags uint32
		entries      [128]struct {
			principal     [16]byte
			flags, rights uint32
		}
	}
}

type getattrlistFunc func(*byte, *unix.Attrlist, *aclAttributes, uintptr, uint32) int32

// libSystem is linked on Darwin, including in the cgo-free release builds.
var loadGetattrlist = sync.OnceValue(func() getattrlistFunc {
	symbol, err := purego.Dlsym(purego.RTLD_DEFAULT, "getattrlist")
	if err != nil {
		return nil
	}

	var fn getattrlistFunc
	purego.RegisterFunc(&fn, symbol)
	return fn
})

func checkACL(path string, info os.FileInfo) error {
	name, err := unix.BytePtrFromString(path)
	query := loadGetattrlist()
	if err != nil || query == nil {
		return ErrUnavailable
	}

	attrs := unix.Attrlist{
		Bitmapcount: unix.ATTR_BIT_MAP_COUNT,
		Commonattr:  unix.ATTR_CMN_DEVID | unix.ATTR_CMN_EXTENDED_SECURITY | unix.ATTR_CMN_FILEID,
	}
	var result aclAttributes
	if query(name, &attrs, &result, unsafe.Sizeof(result), unix.FSOPT_NOFOLLOW|unix.FSOPT_REPORT_FULLSIZE) != 0 {
		return ErrUnavailable
	}

	want := info.Sys().(*syscall.Stat_t)
	if result.device != want.Dev || result.inode != want.Ino {
		return ErrUnavailable
	}

	const fixed = unsafe.Offsetof(result.security)
	if result.aclOffset != int32(fixed-unsafe.Offsetof(result.aclOffset)) ||
		uintptr(result.length) != fixed+uintptr(result.aclLength) || uintptr(result.length) > unsafe.Sizeof(result) {
		return ErrUnavailable
	}
	size := uintptr(result.aclLength)
	if size == 0 {
		return nil
	}

	sec := &result.security
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
