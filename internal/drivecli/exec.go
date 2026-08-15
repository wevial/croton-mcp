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

package drivecli

import "os"

func isolatedEnvironment(cacheDir, credentialsStore string) []string {
	environment := []string{"PATH=/usr/bin:/bin"}
	for _, key := range []string{"HOME", "USER", "LOGNAME", "LANG", "LC_ALL"} {
		if value, ok := os.LookupEnv(key); ok {
			environment = append(environment, key+"="+value)
		}
	}
	if cacheDir != "" {
		environment = append(environment, "PROTON_DRIVE_CACHE_DIR="+cacheDir)
	}
	if credentialsStore != "" {
		environment = append(environment, "PROTON_DRIVE_CREDENTIALS_STORE="+credentialsStore)
	}

	return environment
}

type limitedBuffer struct {
	bytes    []byte
	limit    int
	overflow bool
}

func (buffer *limitedBuffer) Write(input []byte) (int, error) {
	if len(input) > buffer.limit-len(buffer.bytes) {
		buffer.overflow = true
		return len(input), nil
	}

	buffer.bytes = append(buffer.bytes, input...)
	return len(input), nil
}
