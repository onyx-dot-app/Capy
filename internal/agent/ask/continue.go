package ask

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ErrNoConversation is a thread its agent never started a conversation
// for: there is nothing to continue.
var ErrNoConversation = errors.New("ask: the thread has no conversation to continue")

// ContinueIn opens Terminal on thread id's conversation in its agent
// itself, with the user's own settings and Capy's tools: a script in
// ContinueDir that cds there and execs the agent's command, every value
// single-quoted.
func (a *Ask) ContinueIn(ctx context.Context, id string) error {
	d, err := a.Thread(ctx, id)
	if err != nil {
		return err
	}
	if d.ConversationID == "" {
		return ErrNoConversation
	}
	argv, err := a.o.Gateway.ContinueCommand(d.Agent, d.ConversationID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(a.o.ContinueDir, 0o700); err != nil {
		return errors.New("ask: cannot create the Continue in folder")
	}
	path, err := writeScript(a.o.ContinueDir, continueScript(a.o.ContinueDir, argv))
	if err != nil {
		return err
	}
	return a.o.Open(path)
}

// continueScript cds to dir and execs argv.
func continueScript(dir string, argv []string) string {
	quoted := make([]string, len(argv))
	for i, arg := range argv {
		quoted[i] = quote(arg)
	}
	return "#!/bin/sh\ncd " + quote(dir) + " && exec " + strings.Join(quoted, " ") + "\n"
}

// quote is s single-quoted for sh: nothing in it is interpreted.
func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// writeScript writes continue.command in dir, 0700, through a new file
// renamed over it.
func writeScript(dir, script string) (string, error) {
	path := filepath.Join(dir, "continue.command")
	tmp, err := os.CreateTemp(dir, "continue.*")
	if err != nil {
		return "", errors.New("ask: cannot write the Continue in script")
	}
	defer os.Remove(tmp.Name()) // once renamed, there is nothing to remove
	_, err = tmp.WriteString(script)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp.Name(), 0o700)
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		return "", errors.New("ask: cannot write the Continue in script")
	}
	return path, nil
}

// openInTerminal opens a .command file in Terminal, which runs it: no
// Apple Events permission is needed.
func openInTerminal(path string) error {
	if err := exec.Command("open", "-a", "Terminal", path).Run(); err != nil {
		return errors.New("ask: cannot open Terminal")
	}
	return nil
}
