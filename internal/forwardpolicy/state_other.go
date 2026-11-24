//go:build !linux && !darwin

package forwardpolicy

import "os"

func checkDirectory(string, bool) error      { return ErrUnavailable }
func lockDirectory(string) (*os.File, error) { return nil, ErrUnavailable }
func openFile(string, int) (*os.File, error) { return nil, ErrUnavailable }
