// Package faketool writes stand-ins for agents' command-line tools, for
// tests: shell scripts that answer by their arguments as claude and codex
// do, and record every call.
package faketool

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// script answers --version from <self>.version; mcp get from <self>.get,
// else exits 1 as codex does for a missing server; other mcp commands
// exit with <self>.exit (0 without it); debug from <self>.debug. Every
// call is a line of <self>.log, its arguments separated by \x1f.
const script = `#!/bin/sh
for a in "$@"; do printf '%s\037' "$a"; done >> "$0.log"
printf '\n' >> "$0.log"
case "$1" in
--version) cat "$0.version" ;;
mcp)
	if [ "$2" = get ]; then
		if [ -f "$0.get" ]; then cat "$0.get"; else echo "Error: No MCP server named 'capy' found." >&2; exit 1; fi
	else
		exit "$(cat "$0.exit" 2>/dev/null || echo 0)"
	fi ;;
debug) cat "$0.debug" ;;
*) exit 2 ;;
esac
`

// Write writes an executable at path with body.
func Write(t *testing.T, path, body string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// Tool writes a fake agent tool at path that prints version.
func Tool(t *testing.T, path, version string) string {
	t.Helper()
	Write(t, path, script)
	Set(t, path, "version", version+"\n")
	return path
}

// Set writes the tool's answer of kind: "version", "get", "exit" or
// "debug".
func Set(t *testing.T, tool, kind, content string) {
	t.Helper()
	if err := os.WriteFile(tool+"."+kind, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Calls returns the arguments of every call of the tool, oldest first.
func Calls(t *testing.T, tool string) [][]string {
	t.Helper()
	b, err := os.ReadFile(tool + ".log")
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var calls [][]string
	for line := range strings.Lines(string(b)) {
		args := strings.Split(strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\x1f"), "\x1f")
		if len(args) == 1 && args[0] == "" {
			args = nil
		}
		calls = append(calls, args)
	}
	return calls
}
