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
	// Each single component is opened relative to the retained predecessor.
	// O_NOFOLLOW is mandatory for every open, O_DIRECTORY for directories.
	return unix.Openat(parent, name, flags, mode)
}
