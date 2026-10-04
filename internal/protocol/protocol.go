// Package protocol defines the small wire format clipd speaks over its
// listening socket.
//
// There are two kinds of message, and which one arrived is decided by the
// first bytes on the connection:
//
//	<anything else>   raw clipboard content, written verbatim
//	Magic + JSON line a structured request, dispatched on its "type"
//
// The raw case is the default on purpose. It is what makes `nc` a complete
// client: a shell pipeline that knows nothing about clipd can still put text
// on the clipboard. The structured case exists for everything that needs to
// say more than "here are some bytes" — today that is only file drops.
//
// The design is lifted from wincent/clipper, which solved the same problem
// first: a magic prefix keeps a framed extension from breaking the unframed
// clients that came before it, and versioning the prefix itself leaves room
// to change the frame later without having to guess what an old peer meant.
//
// This package contains no networking policy: no dials, no deadlines, no
// reads from a socket. It parses and formats, and the server decides how long
// anything is allowed to take.
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

// Magic introduces a structured request.
//
// The trailing v1 versions the frame, not the product: a future incompatible
// frame uses a different prefix, and this one keeps meaning exactly what it
// means today. A connection whose opening bytes are anything else is raw
// clipboard content and must stay that way, or every `nc` alias in the wild
// breaks at once.
const Magic = "clipd:magic:v1\n"

// MaxFrameBytes bounds the JSON line following the magic prefix.
//
// The envelope describes a request; it never carries the payload. Anything
// approaching this size is a peer trying to make the daemon buffer without
// bound, not a request, so the read stops rather than growing.
const MaxFrameBytes = 8 << 10

// Request types.
const (
	// TypeDrop is a file transfer: the JSON line is followed by a tar stream
	// on the same connection.
	TypeDrop = "drop"

	// TypeStreamDrop is a versioned body extension; legacy drop bodies stay unchanged.
	TypeStreamDrop = "drop-stream-v2"

	// TypePing asks the listener to identify itself and does nothing else.
	//
	// It exists because the obvious way to ask "is a daemon there" — connect
	// and close — is indistinguishable on the wire from a raw clipboard
	// message of zero bytes, so it used to clear the clipboard every time
	// `clipd status` ran. A request the daemon can recognise answers the
	// question without touching anything, and answers a second one for free:
	// whether the thing listening is clipd at all.
	TypePing = "ping"
)

// Ping is the complete byte sequence a client sends for a TypePing request.
//
// A literal rather than something marshalled, because both callers send
// exactly this and a fixed slice cannot fail to encode.
var Ping = []byte(Magic + `{"type":"ping"}` + "\n")

// Pong is the message a daemon answers a Ping with, after StatusOK.
const Pong = "pong"

// Response status prefixes.
//
// The daemon's reply is plain text so that an unmodified `nc` displays
// something useful, but the first token is machine-readable so the generated
// shell function can turn a result into an exit status. Without it every drop
// exited 0, including the ones the daemon rejected.
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
	// Empty means the body is a tar stream and the names come from the archive.
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
