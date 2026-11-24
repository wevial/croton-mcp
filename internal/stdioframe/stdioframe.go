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

// Package stdioframe is the bounded newline-delimited stdio transport shared
// by every Croton executable, so they cannot diverge on what a frame is.
package stdioframe

import (
	"bufio"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wevial/croton-mcp/internal/strictjson"
)

// MaxFrameBytes is the ceiling on one newline-delimited stdio frame,
// including its terminating newline.
const MaxFrameBytes = 64 * 1024

// MaxForwardIDBytes accommodates a quoted UUID on both send_forward calls.
const MaxForwardIDBytes = 38

// ForwardMetaBytesHeader carries a stdio-only measurement in SDK RequestExtra.
// Its value is local to the request and never appears on the wire.
const ForwardMetaBytesHeader = "Croton-Forward-Meta-Bytes"

var (
	errStdioFrameTooLarge = errors.New("stdio frame exceeds limit")
	errStdioFrameInvalid  = errors.New("stdio frame is invalid")
)

// NewTransport creates Croton's bounded newline-delimited stdio transport.
// Each complete input frame is admitted only after a bounded reader has
// proven it is one unambiguous JSON object within the ceiling; oversize or
// ambiguous frame fragments never reach the SDK. Standard output is kept
// exclusively for SDK JSON-RPC frames and is never closed by the SDK.
func NewTransport(stdin io.ReadCloser, stdout io.Writer) mcp.Transport {
	return stdioTransport{mcp.IOTransport{
		Reader: &boundedReadCloser{
			Reader: newBoundedFrameReader(stdin, MaxFrameBytes),
			Closer: stdin,
		},
		Writer: nopWriteCloser{Writer: stdout},
	}}
}

type stdioTransport struct{ mcp.IOTransport }

func (t stdioTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	conn, err := t.IOTransport.Connect(ctx)
	if err != nil {
		return nil, err
	}

	return stdioConnection{conn}, nil
}

type stdioConnection struct{ mcp.Connection }

func (c stdioConnection) Read(ctx context.Context) (jsonrpc.Message, error) {
	msg, err := c.Connection.Read(ctx)
	if err != nil {
		return msg, err
	}

	// A legacy elicitation reply carries the form directly in result.
	if reply, ok := msg.(*jsonrpc.Response); ok {
		if !canonicalFormFields(jsontext.Value(reply.Result)) {
			return nil, errStdioFrameInvalid
		}
		return msg, nil
	}
	req, ok := msg.(*jsonrpc.Request)
	if !ok || req.Method != "tools/call" {
		return msg, nil
	}

	// Params still contains the original JSON here. Tool handlers receive
	// _meta as a map, which loses number spellings, whitespace and escapes.
	var params struct {
		Name           string                    `json:"name"`
		Meta           jsontext.Value            `json:"_meta"`
		InputResponses map[string]jsontext.Value `json:"inputResponses"`
	}
	if err := json.Unmarshal(req.Params, &params, jsontext.AllowInvalidUTF8(true)); err != nil {
		return nil, errStdioFrameInvalid
	}
	if params.Name != "send_forward" && params.Name != "prepare_forward" {
		return msg, nil
	}
	if params.Name == "send_forward" {
		for _, form := range params.InputResponses {
			if !canonicalFormFields(form) {
				return nil, errStdioFrameInvalid
			}
		}
	}

	req.Extra = &mcp.RequestExtra{Header: http.Header{
		ForwardMetaBytesHeader: {strconv.Itoa(len(params.Meta))},
	}}
	return msg, nil
}

// The SDK folds struct field names when decoding elicitation results.
// Form content and protocol metadata remain case-sensitive open maps.
func canonicalFormFields(raw jsontext.Value) bool {
	if raw.Kind() != '{' {
		return true
	}

	var fields map[string]jsontext.Value
	if json.Unmarshal(raw, &fields, jsontext.AllowInvalidUTF8(true)) != nil {
		return false
	}
	for key := range fields {
		if (key != "action" && strings.EqualFold(key, "action")) ||
			(key != "content" && strings.EqualFold(key, "content")) ||
			(key != "_meta" && strings.EqualFold(key, "_meta")) {
			return false
		}
	}
	return true
}

type boundedReadCloser struct {
	io.Reader
	io.Closer
}

type nopWriteCloser struct {
	io.Writer
}

func (nopWriteCloser) Close() error { return nil }

type boundedFrameReader struct {
	reader      *bufio.Reader
	maximum     int
	pending     []byte
	terminalErr error
}

func newBoundedFrameReader(reader io.Reader, maximum int) io.Reader {
	if maximum <= 0 {
		return &boundedFrameReader{terminalErr: errStdioFrameTooLarge}
	}

	return &boundedFrameReader{
		reader:  bufio.NewReaderSize(reader, maximum+1),
		maximum: maximum,
	}
}

func (reader *boundedFrameReader) Read(destination []byte) (int, error) {
	if len(destination) == 0 {
		return 0, nil
	}
	if len(reader.pending) == 0 {
		if reader.terminalErr != nil {
			return 0, reader.terminalErr
		}

		frame, err := reader.reader.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) || len(frame) > reader.maximum {
			reader.terminalErr = errStdioFrameTooLarge
			return 0, reader.terminalErr
		}
		if err != nil && !errors.Is(err, io.EOF) {
			reader.terminalErr = err
			return 0, err
		}
		if len(frame) == 0 {
			reader.terminalErr = err
			return 0, err
		}
		if !strictjson.ValidateObject(frame, reader.maximum) || oversizedForwardID(frame) {
			reader.terminalErr = errStdioFrameInvalid
			return 0, reader.terminalErr
		}

		reader.pending = append(reader.pending[:0], frame...)
		if errors.Is(err, io.EOF) {
			reader.terminalErr = io.EOF
		}
	}

	count := copy(destination, reader.pending)
	reader.pending = reader.pending[count:]
	return count, nil
}

// Check the wire token here, before the SDK decodes escapes and strips the ID
// from the request passed to the tool handler.
func oversizedForwardID(frame []byte) bool {
	var call struct {
		ID     jsontext.Value `json:"id"`
		Method string         `json:"method"`
		Params struct {
			Name string `json:"name"`
		} `json:"params"`
	}
	return json.Unmarshal(frame, &call, jsontext.AllowInvalidUTF8(true)) == nil && call.Method == "tools/call" &&
		call.Params.Name == "send_forward" && len(call.ID) > MaxForwardIDBytes
}
