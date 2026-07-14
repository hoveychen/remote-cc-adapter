// Package execproto is the streaming wire format between the subprocess proxy
// (`rca _spawn-proxy`, exec'd in place of a subprocess by the native
// interceptor) and the executor's subprocess service.
//
// It mirrors the POC's remote_run.py <-> exec_server.py protocol (design doc
// §4.1 point 4), promoted to a typed Go codec:
//
//	proxy -> executor : one framed SpawnRequest (uint32 len + JSON), then zero or
//	                    more control frames — signal forwarding and stdin chunks
//	                    (a zero-length stdin frame signals EOF).
//	executor -> proxy : a stream of tagged frames — stdout/stderr chunks, then a
//	                    final exit frame carrying the child's exit code.
//
// Tagged frame layout: [1 byte tag][uint32 big-endian len][len bytes payload].
// The exit frame's payload is a 4-byte big-endian int32 exit code.
package execproto

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
)

// Frame tags.
const (
	TagStdout byte = 'O' // executor -> proxy: stdout chunk
	TagStderr byte = 'E' // executor -> proxy: stderr chunk
	TagExit   byte = 'X' // executor -> proxy: final exit code (int32)
	TagSignal byte = 'S' // proxy -> executor: forward a signal (int32 signum)
	TagStdin  byte = 'I' // proxy -> executor: stdin chunk; a zero-length frame means EOF
)

// MaxChunk caps a single stream chunk.
const MaxChunk = 1 << 20 // 1 MiB

// SpawnRequest describes a subprocess the executor should run on its host.
//
// Path is the binary to execute. Argv is the full argument vector INCLUDING
// argv[0], preserved separately from Path because some binaries pick their mode
// from argv[0] — claude runs its embedded ripgrep only when argv[0]'s basename
// is "rg". If Path is empty, the executor falls back to Argv[0] as the binary
// path (older proxy behaviour).
type SpawnRequest struct {
	Path string   `json:"path,omitempty"`
	Argv []string `json:"argv"`
	Cwd  string   `json:"cwd"`
	Env  []string `json:"env,omitempty"`
}

// WriteSpawnRequest sends the length-prefixed JSON header.
func WriteSpawnRequest(w io.Writer, req *SpawnRequest) error {
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(body)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err = w.Write(body)
	return err
}

// ReadSpawnRequest reads the length-prefixed JSON header.
func ReadSpawnRequest(r io.Reader) (*SpawnRequest, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > MaxChunk*16 {
		return nil, fmt.Errorf("execproto: spawn header too large: %d", n)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	var req SpawnRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, err
	}
	return &req, nil
}

// WriteChunk sends a stdout/stderr chunk.
func WriteChunk(w io.Writer, tag byte, data []byte) error {
	if len(data) > MaxChunk {
		data = data[:MaxChunk]
	}
	var hdr [5]byte
	hdr[0] = tag
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(data)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(data)
	return err
}

// WriteExit sends the final exit-code frame.
func WriteExit(w io.Writer, code int32) error {
	var buf [9]byte
	buf[0] = TagExit
	binary.BigEndian.PutUint32(buf[1:5], 4)
	binary.BigEndian.PutUint32(buf[5:9], uint32(code))
	_, err := w.Write(buf[:])
	return err
}

// WriteSignal sends a signal-forwarding frame (proxy -> executor).
func WriteSignal(w io.Writer, signum int32) error {
	var buf [9]byte
	buf[0] = TagSignal
	binary.BigEndian.PutUint32(buf[1:5], 4)
	binary.BigEndian.PutUint32(buf[5:9], uint32(signum))
	_, err := w.Write(buf[:])
	return err
}

// Frame is a decoded tagged frame.
type Frame struct {
	Tag  byte
	Data []byte
}

// ReadFrame reads one tagged frame.
func ReadFrame(r io.Reader) (*Frame, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > MaxChunk {
		return nil, fmt.Errorf("execproto: chunk too large: %d", n)
	}
	data := make([]byte, n)
	if _, err := io.ReadFull(r, data); err != nil {
		return nil, err
	}
	return &Frame{Tag: hdr[0], Data: data}, nil
}

// ExitCode decodes an exit frame's payload.
func (f *Frame) ExitCode() int32 {
	if f.Tag != TagExit || len(f.Data) < 4 {
		return -1
	}
	return int32(binary.BigEndian.Uint32(f.Data))
}

// Signum decodes a signal frame's payload.
func (f *Frame) Signum() int32 {
	if f.Tag != TagSignal || len(f.Data) < 4 {
		return 0
	}
	return int32(binary.BigEndian.Uint32(f.Data))
}
