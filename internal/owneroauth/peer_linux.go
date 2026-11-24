package owneroauth

import "golang.org/x/sys/unix"

func ownerPeer(fd int, uid int) bool {
	cred, err := unix.GetsockoptUcred(fd, unix.SOL_SOCKET, unix.SO_PEERCRED)
	return err == nil && int(cred.Uid) == uid
}
