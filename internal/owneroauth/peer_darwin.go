package owneroauth

import "golang.org/x/sys/unix"

func ownerPeer(fd int, uid int) bool {
	cred, err := unix.GetsockoptXucred(fd, unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	// Darwin getpeereid expects XUCRED_VERSION (zero).
	return err == nil && cred.Version == 0 && int(cred.Uid) == uid
}
