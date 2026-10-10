// Package launchagent installs clipd as a per-user macOS LaunchAgent.
//
// A LaunchAgent, not a LaunchDaemon. A LaunchDaemon runs as root in the
// system domain with no connection to any user's GUI session, and the macOS
// pasteboard belongs to a GUI session: pbcopy invoked from a LaunchDaemon
// either fails outright or writes somewhere the logged-in user will never
// paste from. A LaunchAgent bootstrapped into gui/<uid> runs as the user,
// inside their session, with exactly the pasteboard access `pbcopy` has when
// they run it in Terminal. It also means none of this needs root.
//
// Plist generation lives in this file with no build tag so it can be tested
// on any platform; the launchctl interaction is darwin-only.
package launchagent

import (
	"encoding/xml"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Label is the launchd service label. It doubles as the plist filename, which
// launchd requires to match.
const Label = "com.clipd.agent"

// ErrUnsupported reports that launchd exists only on macOS. On Linux clipd is
// a client and has nothing to install: the binary on PATH is the whole
// deployment. It is declared here, untagged, so callers can compare against
// it on every platform.
var ErrUnsupported = errors.New("LaunchAgent management is only available on macOS")

// Spec is the subset of a launchd job description clipd needs.
type Spec struct {
	Label            string
	ProgramArguments []string
	RunAtLoad        bool
	KeepAlive        bool
	StandardOutPath  string
	StandardErrPath  string
	// EnvironmentVariables is used to pin a non-default config path, since a
	// LaunchAgent does not inherit the user's shell environment.
	EnvironmentVariables map[string]string
}

// Marshal renders the spec as a launchd property list.
//
// The plist is written by hand rather than through encoding/xml because
// plist's alternating <key>/<value> layout does not map onto Go structs
// without a purpose-built marshaller. Every interpolated value is escaped, so
// a path containing & or < cannot produce a corrupt plist.
func (s Spec) Marshal() ([]byte, error) {
	if s.Label == "" {
		return nil, fmt.Errorf("launchagent: empty label")
	}
	if len(s.ProgramArguments) == 0 {
		return nil, fmt.Errorf("launchagent: no program arguments")
	}

	var b strings.Builder
	b.WriteString(xml.Header)
	b.WriteString("<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n")
	b.WriteString("<plist version=\"1.0\">\n<dict>\n")

	writeString(&b, "Label", s.Label)

	b.WriteString("\t<key>ProgramArguments</key>\n\t<array>\n")
	for _, arg := range s.ProgramArguments {
		b.WriteString("\t\t<string>")
		writeEscaped(&b, arg)
		b.WriteString("</string>\n")
	}
	b.WriteString("\t</array>\n")

	writeBool(&b, "RunAtLoad", s.RunAtLoad)
	writeBool(&b, "KeepAlive", s.KeepAlive)

	// Background tells launchd this job is not user-interactive, so it is
	// scheduled with lower priority and is throttled appropriately. It is
	// also what keeps it from ever putting anything on screen.
	writeString(&b, "ProcessType", "Background")

	if s.StandardOutPath != "" {
		writeString(&b, "StandardOutPath", s.StandardOutPath)
	}
	if s.StandardErrPath != "" {
		writeString(&b, "StandardErrorPath", s.StandardErrPath)
	}

	if len(s.EnvironmentVariables) > 0 {
		b.WriteString("\t<key>EnvironmentVariables</key>\n\t<dict>\n")
		// Sorted so the generated plist is byte-stable across runs, which
		// keeps reinstalls from churning the file and makes it diffable.
		keys := make([]string, 0, len(s.EnvironmentVariables))
		for k := range s.EnvironmentVariables {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			b.WriteString("\t\t<key>")
			writeEscaped(&b, k)
			b.WriteString("</key>\n\t\t<string>")
			writeEscaped(&b, s.EnvironmentVariables[k])
			b.WriteString("</string>\n")
		}
		b.WriteString("\t</dict>\n")
	}

	b.WriteString("</dict>\n</plist>\n")
	return []byte(b.String()), nil
}

func writeString(b *strings.Builder, key, value string) {
	b.WriteString("\t<key>")
	writeEscaped(b, key)
	b.WriteString("</key>\n\t<string>")
	writeEscaped(b, value)
	b.WriteString("</string>\n")
}

func writeBool(b *strings.Builder, key string, value bool) {
	b.WriteString("\t<key>")
	writeEscaped(b, key)
	if value {
		b.WriteString("</key>\n\t<true/>\n")
	} else {
		b.WriteString("</key>\n\t<false/>\n")
	}
}

// writeEscaped XML-escapes into the builder. xml.EscapeText writes to an
// io.Writer and strings.Builder never returns a write error, so the error is
// genuinely impossible here rather than merely unlikely.
func writeEscaped(b *strings.Builder, s string) {
	_ = xml.EscapeText(b, []byte(s))
}
