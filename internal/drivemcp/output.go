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

package drivemcp

import "encoding/json"

// maxToolResultBytes caps every serialized tool result at the same output
// budget as the Mail server's tool surface.
const maxToolResultBytes = 100000

// shrinkable results reduce their own content structurally, one step at a
// time, so truncation always re-marshals complete JSON values.
type shrinkable interface {
	shrinkForOutput() bool
}

// encodeBounded serializes a tool result within maxToolResultBytes. Oversize
// results are shrunk structurally and re-marshaled; serialized JSON is never
// byte-sliced, so output remains syntactically valid.
func encodeBounded(result any) ([]byte, bool, error) {
	truncated := false
	for {
		encoded, err := json.Marshal(result)
		if err != nil {
			return nil, false, err
		}
		if len(encoded) <= maxToolResultBytes {
			return encoded, truncated, nil
		}

		shrinker, ok := result.(shrinkable)
		if !ok || !shrinker.shrinkForOutput() {
			return []byte(`{"truncated":true}`), true, nil
		}

		truncated = true
	}
}

func (result *listDriveResult) shrinkForOutput() bool {
	switch {
	case len(result.Entries) > 0:
		result.Entries = result.Entries[:len(result.Entries)/2]
	case len(result.Devices) > 0:
		result.Devices = result.Devices[:len(result.Devices)/2]
	case len(result.Sections) > 0:
		result.Sections = result.Sections[:len(result.Sections)/2]
	default:
		return false
	}

	result.Truncated = true
	return true
}

func (result *sharingStatusResult) shrinkForOutput() bool {
	switch {
	case len(result.ProtonInvitations) >= len(result.NonProtonInvitations) && len(result.ProtonInvitations) >= len(result.Members) && len(result.ProtonInvitations) > 0:
		result.ProtonInvitations = result.ProtonInvitations[:len(result.ProtonInvitations)/2]
	case len(result.NonProtonInvitations) >= len(result.Members) && len(result.NonProtonInvitations) > 0:
		result.NonProtonInvitations = result.NonProtonInvitations[:len(result.NonProtonInvitations)/2]
	case len(result.Members) > 0:
		result.Members = result.Members[:len(result.Members)/2]
	case result.URLAccess != nil:
		result.URLAccess = nil
	default:
		return false
	}

	result.Truncated = true
	return true
}
