// Copyright 2026 Ko
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package drivefs

import "golang.org/x/sys/unix"

func platformOpen(parent int, name string, flags int, mode uint32) (int, error) {
	// The sole absolute open pins the filesystem root. All allowed-root and
	// destination components use the same required constrained primitive.
	if parent == unix.AT_FDCWD && name == "/" {
		return unix.Open(name, flags, mode)
	}

	return unix.Openat2(parent, name, &unix.OpenHow{
		Flags: uint64(flags), Mode: uint64(mode),
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS,
	})
}
