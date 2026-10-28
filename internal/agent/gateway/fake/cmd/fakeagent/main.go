// Command fakeagent stands in for claude, or codex when it is named
// codex, in tests: see package fake.
package main

import (
	"os"
	"path/filepath"

	"github.com/onyx-dot-app/Capy/internal/agent/gateway/fake"
)

func main() {
	os.Exit(fake.Main(os.Args[1:], filepath.Base(os.Args[0]) == "codex", os.Stdout, os.Stderr))
}
