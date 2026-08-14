//go:build !linux && !darwin

package config

import "os"

func ownedByCurrentUser(os.FileInfo) bool { return false }
