//go:build linux || darwin

package config

import (
	"os"
	"syscall"
	"testing"
)

type fileInfoWithStat struct {
	os.FileInfo
	stat *syscall.Stat_t
}

func (info fileInfoWithStat) Sys() any { return info.stat }

func TestOwnedByCurrentUserRejectsForeignUID(t *testing.T) {
	path := t.TempDir()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("unexpected stat type %T", info.Sys())
	}
	current := *stat
	current.Uid = uint32(os.Geteuid())
	if !ownedByCurrentUser(fileInfoWithStat{FileInfo: info, stat: &current}) {
		t.Fatal("current-user ownership was rejected")
	}
	foreign := current
	foreign.Uid++
	if ownedByCurrentUser(fileInfoWithStat{FileInfo: info, stat: &foreign}) {
		t.Fatal("foreign ownership was accepted")
	}
}
