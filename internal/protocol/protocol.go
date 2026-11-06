// Package protocol parses clipboard and structured requests. Input beginning
// with Magic carries a JSON envelope; other input is raw clipboard data.
// The server supplies connection policy and deadlines.
package protocol

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// Magic identifies the v1 envelope. Non-magic input remains raw clipboard data.
const Magic = "clipd:magic:v1\n"

// MaxFrameBytes limits the JSON envelope, excluding payload data.
const MaxFrameBytes = 8 << 10

// Request types.
const (
	// TypeDrop with a Name is one file's raw bytes. Without a Name it was the
	// original archive drop, which the daemon now refuses with instructions
	// to rerun setup, since only outdated shell functions send it.
	TypeDrop = "drop"

	// TypeStreamDrop is an archive drop: a tar stream followed by the
	// completion marker, sent only after tar succeeded.
	TypeStreamDrop = "drop-stream-v2"

	// TypePing identifies the daemon without changing the clipboard.
	TypePing = "ping"
)

// Ping is the complete identification request.
var Ping = []byte(Magic + `{"type":"ping"}` + "\n")

// Pong is the message a daemon answers a Ping with, after StatusOK.
const Pong = "pong"

// Status prefixes are parsed by generated clients and must remain compatible.
const (
	StatusOK    = "clipd: ok: "
	StatusError = "clipd: error: "
)

// Request is the envelope carried on the line after Magic.
//
// Fields beyond Type are per-type and optional; unknown fields are rejected
// rather than ignored, so a client sending something this daemon does not
// understand is told rather than silently half-served.
type Request struct {
	Type string `json:"type"`

	// Name is set on a drop whose body is a single file's raw bytes rather
	// than a tar stream, which is how a pipeline sends output that never
	// existed on disk. It is the sender's suggestion, not an instruction: the
	// daemon reduces it to a bare filename before it becomes a path.
	//
	// It is empty on a TypeStreamDrop, whose names come from the archive.
	Name     string `json:"name,omitempty"`
	Progress bool   `json:"progress,omitempty"`
}

// Sniff reports whether r begins with Magic, consuming the prefix when it
// does and leaving the reader untouched when it does not.
//
// The caller keeps using the same *bufio.Reader either way: the peeked bytes
// live in its buffer, and handing the bare connection to the raw path instead
// would silently drop however much had already been read.
func Sniff(r *bufio.Reader) (bool, error) {
	peek, err := r.Peek(len(Magic))
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	if len(peek) < len(Magic) || string(peek) != Magic {
		return false, nil
	}
	if _, err := r.Discard(len(Magic)); err != nil {
		return false, err
	}
	return true, nil
}

// ReadRequest reads the JSON envelope that follows Magic.
//
// Once the magic prefix has been consumed the connection is committed to the
// structured path: a malformed line is an error, never a fallback to treating
// the bytes as clipboard content. Guessing there would let a peer smuggle
// arbitrary text onto the clipboard by sending a frame it knew would fail.
func ReadRequest(r *bufio.Reader) (Request, error) {
	line, err := readLine(r, MaxFrameBytes)
	if err != nil {
		return Request{}, err
	}
	if len(line) == 0 {
		return Request{}, errors.New("protocol: empty request frame")
	}
	// encoding/json accepts malformed UTF-8 inside a string and replaces each
	// bad byte with U+FFFD. That is unsafe for a filename: the daemon could
	// acknowledge and publish a different name from the one the sender supplied.
	// Reject the frame before decoding so malformed wire bytes are not normalized
	// silently. Raw clipboard streams do not pass through ReadRequest and remain
	// byte-for-byte data.
	if !utf8.Valid(line) {
		return Request{}, errors.New("protocol: request frame is not valid UTF-8")
	}

	// DisallowUnknownFields so a typo in a field name fails loudly instead of
	// leaving a default in place — the same choice the config file makes.
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	var req Request
	if err := dec.Decode(&req); err != nil {
		return Request{}, fmt.Errorf("protocol: parse request: %w", err)
	}
	// One JSON value per frame. Decoding once and stopping would silently
	// accept a second value after the first. Decoder.More reports whether an
	// array or object has another element; it is not a top-level EOF check and
	// incorrectly accepts unmatched closing delimiters such as an extra "}".
	var trailing json.RawMessage
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err != nil {
			return Request{}, fmt.Errorf("protocol: trailing data after the request frame: %w", err)
		}
		return Request{}, errors.New("protocol: trailing data after the request frame")
	}
	if req.Type == "" {
		return Request{}, errors.New("protocol: request has no type")
	}
	return req, nil
}

// readLine reads one newline-terminated line, refusing to buffer more than
// limit bytes. Both "\n" and "\r\n" terminate, because the client may be a
// shell one-liner and printf on some platforms is not fussy about which.
func readLine(r *bufio.Reader, limit int) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := r.ReadSlice('\n')
		buf = append(buf, chunk...)
		if len(buf) > limit {
			return nil, fmt.Errorf("protocol: request frame exceeds %d bytes", limit)
		}
		if err == nil {
			break
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) {
			// A frame that ends at EOF without a newline is still a complete
			// line as far as the sender is concerned; accept it rather than
			// failing on a missing terminator nobody would notice omitting.
			break
		}
		return nil, err
	}
	return []byte(strings.TrimRight(string(buf), "\r\n")), nil
}
