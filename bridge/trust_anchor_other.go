//go:build !linux && !darwin && !freebsd

package bridge

import "os"

const trustAnchorFileSupported = false

func openTrustAnchor(string) (*os.File, error) {
	return nil, os.ErrInvalid
}
