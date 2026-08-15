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

package drivecli_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/wevial/croton-mcp/internal/drivecli"
)

const fixtureSecret = "session-material-must-not-leak@account.test"

func TestHandshakeUsesExplicitVersionOnly(t *testing.T) {
	t.Parallel()

	binary := fakeDriveBinary(t)
	client, err := drivecli.New(drivecli.Options{BinaryPath: binary})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := client.Handshake(context.Background()); err != nil {
		t.Fatalf("Handshake: %v", err)
	}

	argv := recordedArgv(t, binary)
	if argv != "version\n" {
		t.Fatalf("handshake argv = %q, want exact version command", argv)
	}
}

func TestInvokeFailsClosedWithDistinctSecretFreeErrors(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name     string
		scenario string
		timeout  time.Duration
		maxBytes int
		call     func(*testing.T, *drivecli.Client) error
		want     string
	}{
		{
			name:     "timeout",
			scenario: "hang",
			timeout:  40 * time.Millisecond,
			call:     func(_ *testing.T, client *drivecli.Client) error { return client.Handshake(context.Background()) },
			want:     drivecli.CodeTimedOut,
		},
		{
			name:     "stdout overflow",
			scenario: "flood-stdout",
			maxBytes: 256,
			call:     func(_ *testing.T, client *drivecli.Client) error { return client.Handshake(context.Background()) },
			want:     drivecli.CodeOutputOverflow,
		},
		{
			name:     "stderr overflow",
			scenario: "flood-stderr",
			maxBytes: 256,
			call:     func(_ *testing.T, client *drivecli.Client) error { return client.Handshake(context.Background()) },
			want:     drivecli.CodeOutputOverflow,
		},
		{
			name:     "malformed version",
			scenario: "malformed-version",
			call:     func(_ *testing.T, client *drivecli.Client) error { return client.Handshake(context.Background()) },
			want:     drivecli.CodeMalformedOutput,
		},
		{
			name:     "non-zero exit",
			scenario: "nonzero-secret",
			call: func(t *testing.T, client *drivecli.Client) error {
				t.Helper()
				_, err := client.List(context.Background(), "/my-files", "")
				return err
			},
			want: drivecli.CodeCommandFailed,
		},
		{
			name:     "auth required",
			scenario: "auth-required",
			call: func(t *testing.T, client *drivecli.Client) error {
				t.Helper()
				_, err := client.List(context.Background(), "/my-files", "")
				return err
			},
			want: drivecli.CodeAuthRequired,
		},
		{
			name:     "truncated list",
			scenario: "truncated-list",
			call: func(t *testing.T, client *drivecli.Client) error {
				t.Helper()
				_, err := client.List(context.Background(), "/my-files", "")
				return err
			},
			want: drivecli.CodeTruncatedOutput,
		},
		{
			name:     "malformed list",
			scenario: "malformed-list",
			call: func(t *testing.T, client *drivecli.Client) error {
				t.Helper()
				_, err := client.List(context.Background(), "/my-files", "")
				return err
			},
			want: drivecli.CodeMalformedOutput,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			options := drivecli.Options{
				BinaryPath: installFakeDrive(t, testCase.scenario),
				Timeout:    testCase.timeout,
				MaxBytes:   testCase.maxBytes,
			}
			client, err := drivecli.New(options)
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			err = testCase.call(t, client)
			if drivecli.CodeOf(err) != testCase.want {
				t.Fatalf("error = %v, want %q", err, testCase.want)
			}
			if leakedSecret(err) {
				t.Fatalf("error leaked secret material: %q", err)
			}
		})
	}
}

func leakedSecret(err error) bool {
	if err == nil {
		return false
	}

	rendered := fmt.Sprintf("%v %+v %s", err, err, err.Error())
	return strings.Contains(rendered, fixtureSecret) ||
		strings.Contains(rendered, "You need to login first") ||
		strings.Contains(strings.ToLower(rendered), "password")
}
