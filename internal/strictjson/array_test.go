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

package strictjson

import "testing"

func TestDecodeArrayRejectsUnknownFieldsAndTypeMismatch(t *testing.T) {
	t.Parallel()

	type item struct {
		Path string `json:"path"`
	}

	var target []item
	if DecodeArray([]byte(`[{"path":"/my-files","extra":true}]`), 1024, &target) {
		t.Fatal("unknown field was accepted")
	}
	if DecodeArray([]byte(`[{"path":1}]`), 1024, &target) {
		t.Fatal("type mismatch was accepted")
	}
}

func TestDecodeArrayAcceptsEmptyStreamingList(t *testing.T) {
	t.Parallel()

	var target []struct {
		Path string `json:"path"`
	}
	if !DecodeArray([]byte("[\n\n]\n"), 32, &target) {
		t.Fatal("empty streaming list was rejected")
	}
	if len(target) != 0 {
		t.Fatalf("empty list decoded as %#v", target)
	}
}

func TestDecodeObjectAllowsSchemaNullsButStillRejectsUnknownFields(t *testing.T) {
	t.Parallel()

	type author struct {
		OK    bool    `json:"ok"`
		Value *string `json:"value"`
	}
	type node struct {
		UID       string `json:"uid"`
		KeyAuthor author `json:"keyAuthor"`
	}

	var target node
	if !DecodeValue([]byte(`{"uid":"node:1","keyAuthor":{"ok":true,"value":null}}`), 1024, &target) {
		t.Fatal("schema-defined null author was rejected")
	}
	if target.UID != "node:1" || target.KeyAuthor.Value != nil {
		t.Fatalf("decoded node = %#v", target)
	}
	if DecodeValue([]byte(`{"uid":"node:1","keyAuthor":{"ok":true,"value":null},"surprise":1}`), 1024, &target) {
		t.Fatal("unknown field was accepted when nulls are allowed")
	}
}
