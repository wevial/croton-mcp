package forwardpolicy

import "os"

// Linux's group mode bits bound the effective permissions of named ACL entries.
func checkACL(string, os.FileInfo) error { return nil }
