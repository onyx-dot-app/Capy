// Package profile is profile.json: who the user is, in Capy's data
// directory. Highlights marks the lines for them by it, and Ask's agents
// read it for "Was I mentioned?". It imports nothing of Capy, so the
// views, the session, Agent and Capy mcp can all read it.
package profile

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"unicode"
)

// Bounds of a profile.
const (
	MaxNameRunes = 100
	MaxAliases   = 10
	maxFile      = 64 << 10 // far larger than profile.json grows
)

// FileName is the profile's file in Capy's data directory.
const FileName = "profile.json"

// Profile is the user: their name and the other names they go by, as
// nicknames, their team or their projects.
type Profile struct {
	Name    string   `json:"name"`
	Aliases []string `json:"aliases,omitempty"`
}

// Names are the profile's name and aliases, the name first, without
// blanks.
func (p Profile) Names() []string {
	var out []string
	if p.Name != "" {
		out = append(out, p.Name)
	}
	return append(out, p.Aliases...)
}

// normalized keeps what a profile may hold: each name one line of
// printable characters, trimmed and cut to MaxNameRunes, and at most
// MaxAliases other names, without blanks or repeats.
func (p Profile) normalized() Profile {
	name := func(s string) string {
		s = strings.Map(func(r rune) rune {
			if !unicode.IsPrint(r) {
				return -1
			}
			return r
		}, s)
		if r := []rune(s); len(r) > MaxNameRunes {
			s = string(r[:MaxNameRunes])
		}
		return strings.TrimSpace(s)
	}
	out := Profile{Name: name(p.Name)}
	for _, a := range p.Aliases {
		if a = name(a); a != "" && !slices.Contains(out.Aliases, a) && len(out.Aliases) < MaxAliases {
			out.Aliases = append(out.Aliases, a)
		}
	}
	return out
}

// accountName is the full name of the macOS account, "" when it has
// none: a profile not saved yet starts with it.
var accountName = func() string {
	u, err := user.Current()
	if err != nil {
		return ""
	}
	return u.Name
}

// Read reads the profile at path, normalized. A profile not saved yet is
// the account's full name; one that cannot be read is empty.
func Read(path string) Profile {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Profile{Name: accountName()}.normalized()
	}
	if err != nil {
		return Profile{}
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxFile+1))
	var p Profile
	if err != nil || len(b) > maxFile || json.Unmarshal(b, &p) != nil {
		return Profile{}
	}
	return p.normalized()
}

// Save writes p, normalized, to path atomically, 0600, and returns what
// it wrote. Its error never names the path, which may hold the user's
// name for their home folder.
func Save(path string, p Profile) (Profile, error) {
	p = p.normalized()
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return Profile{}, err
	}
	// A new file of a name no other process can have planted, 0600, then
	// renamed over profile.json.
	tmp, err := os.CreateTemp(filepath.Dir(path), FileName+".*")
	if err != nil {
		return Profile{}, pathless(err)
	}
	defer os.Remove(tmp.Name()) // once renamed, there is nothing to remove
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return Profile{}, pathless(err)
	}
	if err := tmp.Close(); err != nil {
		return Profile{}, pathless(err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return Profile{}, pathless(err)
	}
	return p, nil
}

func pathless(err error) error {
	if pe, ok := errors.AsType[*fs.PathError](err); ok {
		return pe.Err
	}
	if le, ok := errors.AsType[*os.LinkError](err); ok {
		return le.Err
	}
	return err
}
