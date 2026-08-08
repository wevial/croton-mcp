package mcpserver

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wevial/croton-mcp/internal/strictjson"
)

const maxStdioFrameBytes = 64 * 1024

var (
	errStdioFrameTooLarge = errors.New("stdio frame exceeds limit")
	errStdioFrameInvalid  = errors.New("stdio frame is invalid")
)

// NewStdioTransport creates Croton's bounded newline-delimited stdio
// transport. Each complete input frame is admitted only after its size is
// proven within the ceiling; oversize frame fragments never reach the SDK.
func NewStdioTransport(stdin io.ReadCloser, stdout io.Writer) mcp.Transport {
	return &mcp.IOTransport{
		Reader: &boundedReadCloser{
			Reader: newBoundedFrameReader(stdin, maxStdioFrameBytes),
			Closer: stdin,
		},
		Writer: nopWriteCloser{Writer: stdout},
	}
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
		if !strictjson.ValidateObject(frame, reader.maximum) || !validProtocolAliases(frame) {
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

// validProtocolAliases rejects case-folded aliases throughout the protocol
// envelope while preserving case-sensitive identifiers in SDK-defined open
// maps. Exact duplicates and depth limits are enforced first by strictjson.
func validProtocolAliases(frame []byte) bool {
	var value any
	if json.Unmarshal(frame, &value) != nil {
		return false
	}
	return validAliasesAt(value, nil)
}

func validAliasesAt(value any, path []string) bool {
	switch typed := value.(type) {
	case map[string]any:
		if openProtocolMap(path) {
			return true
		}
		keys := make([]string, 0, len(typed))
		for key, nested := range typed {
			for _, existing := range keys {
				if strings.EqualFold(existing, key) {
					return false
				}
			}
			keys = append(keys, key)
			if !validAliasesAt(nested, append(path, key)) {
				return false
			}
		}
	case []any:
		for _, nested := range typed {
			if !validAliasesAt(nested, path) {
				return false
			}
		}
	}
	return true
}

func openProtocolMap(path []string) bool {
	if len(path) == 0 {
		return false
	}
	last := path[len(path)-1]
	if strings.EqualFold(last, "_meta") || strings.EqualFold(last, "arguments") {
		return true
	}
	return len(path) >= 2 && strings.EqualFold(path[len(path)-2], "capabilities") &&
		(strings.EqualFold(last, "experimental") || strings.EqualFold(last, "extensions"))
}
