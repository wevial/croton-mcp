//go:build !linux && !darwin

package forwardpolicy

import "os"

func checkDirectory(string, bool) error       { return ErrUnavailable }
func openFile(string, bool) (*os.File, error) { return nil, ErrUnavailable }
