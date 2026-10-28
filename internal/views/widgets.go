// Package views is Capy's native UI, built with mygo: the menu bar item,
// Capy's window, the caption bar and settings.
package views

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/markdown"

	"github.com/onyx-dot-app/Capy/internal/audio"
	"github.com/onyx-dot-app/Capy/internal/keychain"
	"github.com/onyx-dot-app/Capy/internal/lang"
	"github.com/onyx-dot-app/Capy/internal/session"
	"github.com/onyx-dot-app/Capy/internal/views/kit"
	"github.com/onyx-dot-app/Capy/internal/zoomai"
)

// liveSession is what the windows use of a running session.
type liveSession interface {
	Snapshot() session.Snapshot
	SetSpoken(code string) error
	SetTarget(code string) error
	SetTranslate(on bool)
	SetHighlights(cfg session.HighlightConfig)
	Dismiss(ctx context.Context, id string) error
	MergePolish(ctx context.Context) error
	Stop()
	Done() <-chan struct{}
}

// langName is the native name of a language code.
func langName(code string) string {
	if l, ok := lang.Lookup(code); ok {
		return l.Name
	}
	return code
}

// langSelect chooses a language code among options by name, in a pop-up
// button.
func langSelect(c *ui.Context, code *string, options []lang.Language, label string) bool {
	names := make([]string, len(options))
	current := langName(*code)
	for i, l := range options {
		names[i] = l.Name
		if l.Code == *code {
			current = l.Name
		}
	}
	chosen, ok := popUp(c, current, names, label)
	if !ok {
		return false
	}
	for _, l := range options {
		if l.Name == chosen && l.Code != *code {
			*code = l.Code
			return true
		}
	}
	return false
}

// statusText describes a session's status for people.
func statusText(s session.Status) string {
	switch s {
	case session.StatusConnecting:
		return "Connecting…"
	case session.StatusLive:
		return "Listening"
	case session.StatusReconnecting:
		return "Reconnecting…"
	case session.StatusFinishing:
		return "Finishing…"
	case session.StatusSummarizing:
		return "Writing recap…"
	case session.StatusDone:
		return "Recap ready"
	case session.StatusFailed:
		return "Stopped"
	}
	return ""
}

// problem is an error explained for people, with the fix it offers.
type problem struct {
	message string
	detail  string // how to fix it, if there is more to say
	pane    string // System Settings pane to open, if any
	setKey  bool   // the API key is missing or rejected
}

// keyDetail is what an API key needs.
const keyDetail = "Captions need a key with Scribe, Translator and Summarizer access."

func explain(err error) problem {
	var perm *audio.PermissionError
	var limited *zoomai.RateLimitedError
	switch {
	case err == nil:
		return problem{}
	case errors.As(err, &perm) && perm.Pane == audio.MicrophonePane:
		return problem{message: "Capy needs Microphone permission.", detail: "Allow Capy in Privacy & Security › Microphone.", pane: perm.Pane}
	case errors.As(err, &perm) && perm.Pane == audio.SystemAudioPane:
		return problem{message: "Capy needs System Audio Recording permission to hear This Mac.",
			detail: "Allow Capy in Privacy & Security › Screen & System Audio Recording, under System Audio Recording Only.", pane: perm.Pane}
	case errors.As(err, &perm):
		return problem{message: "Capy needs " + perm.What + " permission.", pane: perm.Pane}
	case errors.Is(err, keychain.ErrNotFound):
		return problem{message: "Add your Zoom API key in Settings.", detail: keyDetail, setKey: true}
	case errors.Is(err, zoomai.ErrUnauthorized):
		return problem{message: "Zoom rejected the API key. Check it in Settings.", detail: keyDetail, setKey: true}
	case errors.As(err, &limited):
		return problem{message: "Zoom is rate limiting requests. Try again shortly."}
	case errors.Is(err, audio.ErrUnsupported):
		return problem{message: "Capturing audio needs macOS."}
	}
	return problem{message: strings.TrimPrefix(err.Error(), "session: ")}
}

// captionLangs is "English → 中文", or "English" for captions without translation.
func captionLangs(from, to string, translate bool) string {
	if !translate || from == to {
		return langName(from)
	}
	return langName(from) + " → " + langName(to)
}

// clock formats a time into a session, or its length, as 00:00, or
// 1:02:03 past an hour.
func clock(d time.Duration) string {
	d = max(d, 0)
	h, m, sec := int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, sec)
	}
	return fmt.Sprintf("%02d:%02d", m, sec)
}

// lengthText is a length in words, so that it can't be taken for a time
// of day: "1 min 35 sec", "45 sec", "1 hr 2 min"; parts at zero are left
// out.
func lengthText(d time.Duration) string {
	d = max(d, 0)
	h, m, sec := int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60
	var parts []string
	if h > 0 {
		parts = append(parts, strconv.Itoa(h)+" hr")
	}
	if m > 0 {
		parts = append(parts, strconv.Itoa(m)+" min")
	}
	if sec > 0 || len(parts) == 0 {
		parts = append(parts, strconv.Itoa(sec)+" sec")
	}
	return strings.Join(parts, " ")
}

// markdownText sets Markdown as a document in o, 15-point text with
// 17-point headings: lists, emphasis, links and tables as they read.
func markdownText(c *ui.Context, md string, o *markdown.Options) {
	p := kit.Colors(c)
	h := markdown.HeadingStyle{Size: 17.0 / 15, Weight: 600, Top: 1, Bottom: 0.5}
	o.Theme = &markdown.Theme{FontSize: 15, LineHeight: 1.45, Text: p.Label, Muted: p.Secondary, Link: p.Link,
		BlockSpacing: 0.8, ItemSpacing: 0.4, Headings: [6]markdown.HeadingStyle{h, h, h, h, h, h}}
	markdown.View(c, strings.TrimSpace(md), o).AlignSelf(ui.Stretch)
}
