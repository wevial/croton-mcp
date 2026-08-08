//go:build !linux

package config

import "os"

func ownedByCurrentUser(os.FileInfo) bool { return false }
