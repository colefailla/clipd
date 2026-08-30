package protocol

import (
	"bufio"
	"io"
	"strings"
	"testing"
)

func readerOf(s string) *bufio.Reader { return bufio.NewReader(strings.NewReader(s)) }

// TestSniffLeavesRawStreamsAlone is the compatibility guarantee: anything that
// is not a structured frame must reach the clipboard byte for byte, including
// content that merely resembles the magic prefix.
func TestSniffLeavesRawStreamsAlone(t *testing.T) {
	t.Parallel()

	tests := []string{
		"hello world",
		"",
		"clipd",
		"clipd:magic:v0\n{}\n",
		"clipd:magic:v1 (not followed by a newline)",
		" " + Magic,
	}
	for _, give := range tests {
		r := readerOf(give)
		structured, err := Sniff(r)
		if err != nil {
			t.Errorf("Sniff(%q): %v", give, err)
			continue
		}
		if structured {
			t.Errorf("Sniff(%q) reported a structured frame", give)
			continue
		}
		// Nothing may have been consumed: the raw path gets every byte.
		rest, err := io.ReadAll(r)
		if err != nil {
			t.Fatalf("drain: %v", err)
		}
		if string(rest) != give {
			t.Errorf("Sniff consumed bytes: %q remains, want %q", rest, give)
		}
	}
}

func TestSniffConsumesTheMagicPrefix(t *testing.T) {
	t.Parallel()

	r := readerOf(Magic + `{"type":"drop"}` + "\n" + "tar bytes")
	structured, err := Sniff(r)
	if err != nil {
		t.Fatalf("Sniff: %v", err)
	}
	if !structured {
		t.Fatal("Sniff did not recognise the magic prefix")
	}

	req, err := ReadRequest(r)
	if err != nil {
		t.Fatalf("ReadRequest: %v", err)
	}
	if req.Type != TypeDrop {
		t.Errorf("Type = %q, want %q", req.Type, TypeDrop)
	}

	rest, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if string(rest) != "tar bytes" {
		t.Errorf("body = %q, want the bytes after the frame", rest)
	}
}

func TestReadRequestRejectsBadFrames(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"empty":                 "\n",
		"not json":              "drop\n",
		"no type":               "{}\n",
		"unknown field":         `{"type":"drop","evil":"x"}` + "\n",
		"truncated json":        `{"type":` + "\n",
		"array not object":      `["drop"]` + "\n",
		"second object":         `{"type":"drop"}{"type":"ping"}` + "\n",
		"extra closing brace":   `{"type":"drop"}}` + "\n",
		"extra closing bracket": `{"type":"drop"}]` + "\n",
	}
	for name, give := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := ReadRequest(readerOf(give)); err == nil {
				t.Errorf("ReadRequest(%q) succeeded, want an error", give)
			}
		})
	}
}

// TestReadRequestRejectsInvalidUTF8 pins a surprising encoding/json behavior:
// malformed bytes in a JSON string are otherwise replaced with U+FFFD. For a
// named drop that would publish a silently changed filename.
func TestReadRequestRejectsInvalidUTF8(t *testing.T) {
	t.Parallel()

	give := "{\"type\":\"drop\",\"name\":\"report\xff.txt\"}\n"
	if _, err := ReadRequest(readerOf(give)); err == nil {
		t.Fatal("ReadRequest accepted invalid UTF-8 in a filename")
	} else if !strings.Contains(err.Error(), "valid UTF-8") {
		t.Fatalf("ReadRequest error = %q, want an invalid UTF-8 explanation", err)
	}
}

func TestReadRequestAcceptsUnicode(t *testing.T) {
	t.Parallel()

	const name = "résumé-日本語.txt"
	req, err := ReadRequest(readerOf(`{"type":"drop","name":"` + name + `"}` + "\n"))
	if err != nil {
		t.Fatalf("ReadRequest: %v", err)
	}
	if req.Name != name {
		t.Fatalf("Name = %q, want %q", req.Name, name)
	}
}

// TestReadRequestBoundsTheFrame stops a peer from making the daemon buffer an
// unbounded "envelope" that never ends.
func TestReadRequestBoundsTheFrame(t *testing.T) {
	t.Parallel()

	huge := `{"type":"` + strings.Repeat("a", MaxFrameBytes*2) + `"}` + "\n"
	if _, err := ReadRequest(readerOf(huge)); err == nil {
		t.Fatal("ReadRequest accepted a frame past the limit")
	}
}

func TestReadRequestAcceptsCarriageReturns(t *testing.T) {
	t.Parallel()

	req, err := ReadRequest(readerOf(`{"type":"drop"}` + "\r\n"))
	if err != nil {
		t.Fatalf("ReadRequest: %v", err)
	}
	if req.Type != TypeDrop {
		t.Errorf("Type = %q, want %q", req.Type, TypeDrop)
	}
}

// TestFrameFromTheShellClientParses builds the frame exactly as the generated
// shell function does — printf, then tar bytes — rather than round-tripping a
// Go writer against a Go reader. Nothing ships a Go client, so testing against
// the literal bytes the real client emits is the guarantee that matters.
func TestFrameFromTheShellClientParses(t *testing.T) {
	t.Parallel()

	wire := "clipd:magic:v1\n" + `{"type":"drop"}` + "\n" + "\x00tar\x00bytes"

	r := readerOf(wire)
	structured, err := Sniff(r)
	if err != nil || !structured {
		t.Fatalf("Sniff = %v, %v; want true, nil", structured, err)
	}
	req, err := ReadRequest(r)
	if err != nil {
		t.Fatalf("ReadRequest: %v", err)
	}
	if req.Type != TypeDrop {
		t.Errorf("Type = %q, want %q", req.Type, TypeDrop)
	}
	rest, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if string(rest) != "\x00tar\x00bytes" {
		t.Errorf("body = %q, want the archive bytes untouched", rest)
	}
}
