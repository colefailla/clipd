package launchagent

import (
	"encoding/xml"
	"strings"
	"testing"
)

func TestMarshal(t *testing.T) {
	t.Parallel()

	spec := Spec{
		Label:            Label,
		ProgramArguments: []string{"/usr/local/bin/clipd", "serve"},
		RunAtLoad:        true,
		KeepAlive:        true,
		StandardOutPath:  "/Users/someone/Library/Logs/clipd/clipd.log",
		StandardErrPath:  "/Users/someone/Library/Logs/clipd/clipd.log",
	}

	data, err := spec.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got := string(data)

	for _, want := range []string{
		`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"`,
		`<plist version="1.0">`,
		"<key>Label</key>\n\t<string>com.clipd.agent</string>",
		"<string>/usr/local/bin/clipd</string>",
		"<string>serve</string>",
		"<key>RunAtLoad</key>\n\t<true/>",
		"<key>KeepAlive</key>\n\t<true/>",
		// Background keeps launchd from ever treating this as an interactive
		// job, which is part of "no Terminal window appears".
		"<key>ProcessType</key>\n\t<string>Background</string>",
		"<key>StandardOutPath</key>",
		"<key>StandardErrorPath</key>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("plist is missing %q:\n%s", want, got)
		}
	}

	// launchd will not parse a malformed plist, and the failure mode is a
	// daemon that silently never starts.
	if err := xml.Unmarshal(data, new(struct{})); err != nil {
		t.Errorf("generated plist is not well-formed XML: %v", err)
	}
}

// TestMarshalEscapesValues covers a home directory or binary path containing
// XML metacharacters, which would otherwise produce a corrupt plist.
func TestMarshalEscapesValues(t *testing.T) {
	t.Parallel()

	spec := Spec{
		Label:            Label,
		ProgramArguments: []string{`/Users/a&b/<clipd>`, "serve"},
		StandardOutPath:  `/Users/a&b/log "quoted".log`,
		EnvironmentVariables: map[string]string{
			"CLIPD_CONFIG": `/Users/a&b/config.json`,
		},
	}

	data, err := spec.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got := string(data)

	if strings.Contains(got, "a&b") {
		t.Error("ampersand was not escaped")
	}
	if strings.Contains(got, "<clipd>") {
		t.Error("angle brackets were not escaped")
	}
	if !strings.Contains(got, "&amp;") {
		t.Error("no escaped ampersand in the output")
	}
	if err := xml.Unmarshal(data, new(struct{})); err != nil {
		t.Errorf("escaped plist is not well-formed XML: %v", err)
	}
}

func TestMarshalEnvironmentVariables(t *testing.T) {
	t.Parallel()

	spec := Spec{
		Label:            Label,
		ProgramArguments: []string{"/usr/local/bin/clipd", "serve"},
		EnvironmentVariables: map[string]string{
			"CLIPD_CONFIG": "/tmp/clipd.json",
			"AAA_FIRST":    "1",
		},
	}

	first, err := spec.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(first), "<key>CLIPD_CONFIG</key>") {
		t.Error("CLIPD_CONFIG is missing from the plist")
	}
	// Map iteration order is random; a plist that changes between identical
	// installs would churn the file and defeat diffing it.
	for range 10 {
		again, err := spec.Marshal()
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		if string(again) != string(first) {
			t.Fatal("Marshal is not deterministic across runs")
		}
	}
}

func TestMarshalOmitsEmptyOptionals(t *testing.T) {
	t.Parallel()

	spec := Spec{
		Label:            Label,
		ProgramArguments: []string{"/usr/local/bin/clipd", "serve"},
	}
	data, err := spec.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got := string(data)

	for _, unwanted := range []string{"StandardOutPath", "StandardErrorPath", "EnvironmentVariables"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("plist contains %s despite no value being set", unwanted)
		}
	}
	if !strings.Contains(got, "<key>KeepAlive</key>\n\t<false/>") {
		t.Error("KeepAlive false was not emitted")
	}
}

func TestMarshalRejectsIncompleteSpecs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		spec Spec
	}{
		{"no label", Spec{ProgramArguments: []string{"/usr/local/bin/clipd"}}},
		{"no program arguments", Spec{Label: Label}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := tc.spec.Marshal(); err == nil {
				t.Error("Marshal accepted an incomplete spec")
			}
		})
	}
}
