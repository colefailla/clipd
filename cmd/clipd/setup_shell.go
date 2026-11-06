package main

import (
	"fmt"
	"strings"
)

// staleSocketScript probes the remote forward before removing a refused socket.
// Reply data, diagnostics and exit status are captured separately. macOS nc
// uses a silent-exit heuristic; other transports must report connection refusal.
// The inode check avoids deleting a socket replaced while the probe ran.
func staleSocketScript(t transport) string {
	// One line on purpose: shellFunction indents this script, and an indent
	// inside a pattern spanning two lines would change what it matches.
	refusal := `*": Connection refused"*`
	if t.silentRefusal {
		refusal = `"clipd-probe-status:1:"|` + refusal
	}
	return `_clipd_stale=0
if [ -S "$_clipd_sock" ] && [ ! -L "$_clipd_sock" ]; then
  _clipd_inode=$(command ls -di "$_clipd_sock" 2>/dev/null) || _clipd_inode=
  _clipd_err=$( { {
    if command printf 'clipd:magic:v1\n{"type":"ping"}\n' | LC_ALL=C command ` + t.probe + `; then _clipd_status=0; else _clipd_status=$?; fi
    command printf 'clipd-probe-status:%s:\n' "$_clipd_status" >&2
  } | command sed 's/^/clipd-probe-reply:/' >&2; } 2>&1 ) || :
  case $_clipd_err in
    (*clipd-probe-reply:*) ;;
    (*clipd-probe-status:1:*)
      case $_clipd_err in
        (` + refusal + `)
          if [ -n "$_clipd_inode" ] && [ "$(command ls -di "$_clipd_sock" 2>/dev/null)" = "$_clipd_inode" ] && command rm -f -- "$_clipd_sock"; then
            _clipd_stale=1
          fi ;;
      esac ;;
  esac
fi
`
}

// staleSocketReport explains a socket staleSocketScript removed.
const staleSocketReport = `if [ "$_clipd_stale" = 1 ]; then
  command printf 'clipd: removed the unused socket at %s.\n' "$_clipd_sock" >&2
  command printf '       Log out and reconnect so SSH can create the forward.\n' >&2
  return 1
fi
`

// indent prefixes every line of script.
func indent(script, prefix string) string {
	return prefix + strings.ReplaceAll(strings.TrimSuffix(script, "\n"), "\n", "\n"+prefix) + "\n"
}

// remoteHelp is what `clipd -h` prints on the remote host.
var remoteHelp = []string{
	"Usage:",
	"  command | clipd                          copy command output",
	"  clipd <file> ...                         copy files' contents",
	"  clipd drop [--no-progress] <path> ...    send files and folders",
	"  command | clipd drop --name <filename>   save command output as a file",
	"",
	"Drops go to the receiving computer's configured drop directory",
	"(default: ~/Drop). Folders keep their structure.",
	"",
	"Drops show progress in a terminal. Use --no-progress to hide it.",
	"Run 'clipd status' on the receiving computer to see limits.",
}

// Progress goes to stderr; exactly one final reply determines the exit status.
const shellReplyReader = `(
  _clipd_final=
  _clipd_invalid=0
  while IFS= read -r _clipd_line; do
    case $_clipd_line in
      ('clipd: progress: '*)
        if [ -n "$_clipd_final" ]; then _clipd_invalid=1; fi
        [ "$_clipd_progress" != true ] || command printf '\r%s\033[K' "$_clipd_line" >&2 ;;
      (*)
        if [ -n "$_clipd_final" ]; then _clipd_invalid=1; fi
        _clipd_final=$_clipd_line ;;
    esac
  done
  [ ! -t 2 ] || command printf '\r\033[K' >&2
  if [ "$_clipd_invalid" = 0 ] && [ -n "$_clipd_final" ]; then
    command printf '%s\n' "$_clipd_final"
  else
    command printf 'clipd: error: invalid or missing daemon reply\n'
    exit 1
  fi
)`

// shellFunction isolates scratch variables and traps in a subshell.
func shellFunction(client transport, socket string, hasTar bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", blockStart)
	b.WriteString(`# Sends to the receiving computer through the socket SSH forwards.
# Managed by 'clipd setup'; edits between these markers are overwritten.
`)
	// Escape unalias to bypass aliases while still resolving the Zsh builtin.
	// The guard also allows sourcing under errexit when no alias exists.
	b.WriteString(`\unalias clipd 2>/dev/null || :
clipd() (
`)
	fmt.Fprintf(&b, "  _clipd_sock=%s\n", shellQuote(socket))
	b.WriteString(`  _clipd_reply=
  _clipd_client=0
  _clipd_operation=copy
`)
	b.WriteString("  case ${1:-} in (-h|--help|help)\n    command printf '%s\\n'")
	for _, line := range remoteHelp {
		b.WriteString(" " + shellQuote(line))
	}
	b.WriteString(`
    return 0 ;;
  esac
  _clipd_progress=false
  [ ! -t 2 ] || _clipd_progress=true
`)

	b.WriteString(indent(`if [ ! -S "$_clipd_sock" ]; then
  command printf 'clipd: no SSH forward at %s\n' "$_clipd_sock" >&2
  command printf 'Reconnect using the destination configured with clipd setup.\n' >&2
  return 1
fi
`, "  "))
	// ${1:-} rather than $1, so that a shell running under `set -u` does not
	// abort on a plain `printf x | clipd`, which passes no arguments at all.
	b.WriteString(`  if [ "${1:-}" = "drop" ]; then
    _clipd_operation=drop
    shift
    if [ "${1:-}" = "--no-progress" ]; then _clipd_progress=false; shift; fi
`)
	{
		// --name reads stdin; path operands always go through tar.
		b.WriteString(`    if [ "${1:-}" = "--name" ]; then
      shift
      if [ "$#" -ne 1 ] || [ -z "${1:-}" ]; then
        command printf 'clipd drop: --name needs exactly one filename\n' >&2
        return 64
      fi
      _clipd_name=$1
`)
		// JSON does not permit literal control characters in a string, and the
		// daemon refuses them in filenames anyway. Reject them before framing so a
		// newline cannot turn one request envelope into several lines.
		b.WriteString(`      case $_clipd_name in *'
'*)
        command printf 'clipd drop: the filename contains a control character\n' >&2; return 64 ;;
      esac
      if command printf '%s' "$_clipd_name" | LC_ALL=C command grep '[[:cntrl:]]' >/dev/null 2>&1; then
        command printf 'clipd drop: the filename contains a control character\n' >&2
        return 64
      fi
`)
		// The name lands inside a JSON string, so the two characters JSON
		// escapes have to be escaped here. Everything else the daemon rejects.
		b.WriteString(`      _clipd_esc=$(command printf '%s' "$_clipd_name" | command sed 's/\\/\\\\/g; s/"/\\"/g') || return 1
`)
		fmt.Fprintf(&b, "      if _clipd_reply=$( { command printf 'clipd:magic:v1\\n{\"type\":\"drop\",\"name\":\"%%s\",\"progress\":%%s}\\n' \"$_clipd_esc\" \"$_clipd_progress\"; command cat; } | command %s | ", client.send)
		b.WriteString(shellReplyReader)
		b.WriteString(" ); then :; else _clipd_client=$?; fi\n")
		b.WriteString(`    else
      if [ $# -eq 0 ]; then
        command printf 'clipd drop: no files given.\n' >&2
        command printf "            Use 'clipd drop <file> ...' or 'command | clipd drop --name <filename>'.\n" >&2
        return 64
      fi
`)
		if hasTar {
			// Each operand is reduced to an absolute -C directory and a ./basename.
			// Prefixing operands prevents option injection while allowing multiple
			// tar -C directives (a global -- would turn those into filenames).
			//
			// The socket is checked before tar starts: once the transport is
			// gone, tar's own write errors would bury the reason.
			b.WriteString(indent(staleSocketScript(client)+staleSocketReport, "      "))
			b.WriteString(`      _clipd_reply=$(
        _clipd_first=1
        for _clipd_path in "$@"; do
          case $_clipd_path in (*'
'*) command printf 'clipd drop: path contains a control character\n' >&2; exit 3 ;; esac
          if command printf '%s' "$_clipd_path" | LC_ALL=C command grep '[[:cntrl:]]' >/dev/null 2>&1; then
            command printf 'clipd drop: path contains a control character\n' >&2; exit 3
          fi
          while [ "${_clipd_path%/}" != "$_clipd_path" ]; do _clipd_path=${_clipd_path%/}; done
          _clipd_base=${_clipd_path##*/}
          case $_clipd_base in (''|.|..) command printf 'clipd drop: send a file or folder, not a filesystem root\n' >&2; exit 3 ;; esac
          case $_clipd_path in (*/*) _clipd_parent=${_clipd_path%/*}; [ -n "$_clipd_parent" ] || _clipd_parent=/ ;; (*) _clipd_parent=. ;; esac
          case $_clipd_parent in (/*|./*|../*|.) ;; (*) _clipd_parent=./$_clipd_parent ;; esac
          _clipd_parent=$(command -p sh -c 'CDPATH=; command cd "$1" && command pwd -P' sh "$_clipd_parent") || exit 3
          if [ "$_clipd_first" = 1 ]; then set --; _clipd_first=0; fi
          set -- "$@" -C "$_clipd_parent" "./$_clipd_base"
        done
        {
          command printf 'clipd:magic:v1\n{"type":"drop-stream-v2","progress":%s}\n' "$_clipd_progress"
          if COPYFILE_DISABLE=1 command tar cf - "$@"; then
            command printf 'clipd:complete:v2\n'
          else
            # Above 128 is a signal, normally SIGPIPE from a transport that
            # already failed; the transport failure is what gets reported.
            [ "$?" -gt 128 ] || command printf 'clipd drop: tar failed; no files were saved.\n' >&2
          fi
        } | `)
			fmt.Fprintf(&b, "command %s | ", client.send)
			b.WriteString(shellReplyReader)
			b.WriteString("\n      ) || _clipd_client=$?\n")
		} else {
			b.WriteString(`      command printf 'clipd drop: tar is not installed on this host.\n' >&2
      command printf "            Install tar to send files and folders, or use\n" >&2
      command printf "            'command | clipd drop --name <filename>' to send command output.\n" >&2; return 1
`)
		}
		b.WriteString(`    fi
`)
	}
	// File arguments copy their contents, as `clipd < file` does. Each one is
	// checked before connecting, which catches the common mistakes before the
	// clipboard is touched. A raw copy has no completion marker, so a read that
	// fails partway still reaches the Mac as an ordinary end of input; cat's
	// failure is carried out on fd 3, because a pipeline reports only its last
	// command's status, and a copy the Mac acknowledged is then reported as
	// failed. A failed transport also fails cat, so that case keeps its own
	// handling below.
	b.WriteString(`  elif [ "$#" -gt 0 ]; then
    for _clipd_path in "$@"; do
      if [ -d "$_clipd_path" ]; then
        command printf 'clipd: %s is a directory; use clipd drop to send it\n' "$_clipd_path" >&2
        return 64
      fi
      if [ ! -e "$_clipd_path" ] || [ ! -r "$_clipd_path" ]; then
        command printf 'clipd: cannot read %s\n' "$_clipd_path" >&2
        return 1
      fi
    done
`)
	fmt.Fprintf(&b, "    if _clipd_reply=$( { { command cat -- \"$@\" || command printf 'clipd-read-failed\\n' >&3; } | command %s | ", client.send)
	b.WriteString(shellReplyReader)
	b.WriteString("; } 3>&1 ); then :; else _clipd_client=$?; fi\n")
	b.WriteString(`    case $_clipd_reply in
      (*clipd-read-failed*)
        _clipd_reply=$(command printf '%s\n' "$_clipd_reply" | command sed '/^clipd-read-failed$/d')
        case $_clipd_reply in
          ('clipd: ok: '*)
            command printf "clipd: couldn't read all the files. The clipboard may contain a partial copy.\n" >&2
            return 1 ;;
        esac ;;
    esac
  else
`)
	fmt.Fprintf(&b, "    if _clipd_reply=$(command %s | ", client.send)
	b.WriteString(shellReplyReader)
	b.WriteString(" ); then :; else _clipd_client=$?; fi\n")
	b.WriteString(`  fi
`)
	// A transport failure with a refused connection is a socket sshd left
	// behind; anything else is reported without touching the socket.
	b.WriteString(`  if [ "$_clipd_client" -ne 0 ] || [ -z "$_clipd_reply" ]; then
`)
	b.WriteString(indent(`if [ "$_clipd_client" -ne 3 ]; then
`+indent(staleSocketScript(client)+staleSocketReport, "  ")+`fi
`, "    "))
	b.WriteString(`    if [ -n "$_clipd_reply" ]; then command printf '%s\n' "$_clipd_reply"; fi
    if [ "$_clipd_client" -ne 3 ]; then
      command printf 'clipd: could not confirm the %s.\n' "$_clipd_operation" >&2
      if [ "$_clipd_operation" = drop ]; then
        command printf 'Files may have arrived. Check the destination before retrying.\n' >&2
      fi
      command printf "Run 'clipd status' on the receiving computer.\n" >&2
    fi
    return 1
  fi
`)
	// The daemon's reply is the whole user interface for the result, so it is
	// printed whatever it says; the status prefix on it is what turns the
	// result into an exit code.
	b.WriteString(`  command printf '%s\n' "$_clipd_reply"
  case $_clipd_reply in
    'clipd: ok: '*) return 0 ;;
    *) return 1 ;;
  esac
)
`)
	fmt.Fprintf(&b, "%s\n", blockEnd)
	return b.String()
}

// shellQuote renders one literal POSIX shell word.
//
// Double quotes are insufficient here: a remote home directory containing a
// dollar sign or backtick would still perform expansion when the generated rc
// file is sourced. A single quote is represented by ending the quoted word,
// writing a quoted quote, and starting it again.
func shellQuote(v string) string {
	return "'" + strings.ReplaceAll(v, "'", "'\"'\"'") + "'"
}
