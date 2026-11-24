//go:build !linux && !darwin

package owneroauth

import "os"

func secureDir(string, bool) error                            { return errState }
func openDatabase(string, int, os.FileMode) (*os.File, error) { return nil, errState }
func ownerPeer(int, int) bool                                 { return false }
