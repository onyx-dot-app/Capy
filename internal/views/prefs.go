package views

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"

	"github.com/onyx-dot-app/Capy/internal/lang"
	"github.com/onyx-dot-app/Capy/internal/store"
)

// Display is how the caption bar lays out a turn and its translation.
type Display string

const (
	// DisplayInline shows the translation under what was said.
	DisplayInline Display = "inline"
	// DisplaySideBySide shows what was said on the left and its
	// translation on the right.
	DisplaySideBySide Display = "sideBySide"
)

// Prefs are the settings that are not secret, saved as JSON in the app's
// data directory. The API key lives in the Keychain instead.
type Prefs struct {
	CaptionsSource store.Source `json:"captionsSource"`
	CaptionsFrom   string       `json:"captionsFrom"`
	// CaptionsTranslate turns translation to CaptionsTo on, which stays
	// chosen while it is off.
	CaptionsTranslate bool   `json:"captionsTranslate"`
	CaptionsTo        string `json:"captionsTo"`
	// CaptionsTransparency is how much the caption bar's background lets
	// through, in percent: 0 is solid, 100 shows only the text.
	CaptionsTransparency int `json:"captionsTransparency"`
	// CaptionsDisplay applies to translated captions only: captions in the
	// spoken language have nothing to show beside them.
	CaptionsDisplay Display `json:"captionsDisplay"`
	// ImportLanguage is the language spoken in the files last imported:
	// Speech Summarization does not detect it. ImportTarget is the one
	// their transcripts were translated to, "" for none.
	ImportLanguage string `json:"importLanguage"`
	ImportTarget   string `json:"importTarget"`
	// Highlights marks the turns that matter while recording, where its
	// feature is on: checked every HighlightEvery minutes, of the kinds
	// turned on, How many at More or Fewer, and the user's own drawing
	// attention when HighlightNudge.
	Highlights             bool `json:"highlights"`
	HighlightEvery         int  `json:"highlightEvery"`
	HighlightForYou        bool `json:"highlightForYou"`
	HighlightDecisions     bool `json:"highlightDecisions"`
	HighlightDebates       bool `json:"highlightDebates"`
	HighlightActions       bool `json:"highlightActions"`
	HighlightAnnouncements bool `json:"highlightAnnouncements"`
	HighlightHeadsUps      bool `json:"highlightHeadsUps"`
	HighlightMore          bool `json:"highlightMore"`
	HighlightNudge         bool `json:"highlightNudge"`
	// HighlightNameAsked is the tip asking for the user's name, shown
	// once.
	HighlightNameAsked bool `json:"highlightNameAsked"`
}

// highlightEvery are the intervals Check every offers, in minutes.
var highlightEvery = []int{1, 2, 5, 10}

// DefaultPrefs caption the microphone, detecting the language spoken,
// untranslated (to Chinese once turned on), inline, on a mostly solid bar,
// take imported files to be in English, untranslated, and highlight
// every kind every 5 minutes, Fewer.
func DefaultPrefs() Prefs {
	return Prefs{
		CaptionsSource:       store.SourceMic,
		CaptionsFrom:         lang.Auto,
		CaptionsTo:           "zh-CN",
		CaptionsTransparency: 18,
		CaptionsDisplay:      DisplayInline,
		ImportLanguage:       "en-US",
		Highlights:           true,
		HighlightEvery:       5,
		HighlightForYou:      true, HighlightDecisions: true, HighlightDebates: true, HighlightActions: true,
		HighlightAnnouncements: true, HighlightHeadsUps: true,
		HighlightNudge: true,
	}
}

// normalized replaces unknown values, as from an older or edited file,
// with the defaults.
func (p Prefs) normalized() Prefs {
	d := DefaultPrefs()
	spoken, target := lang.IsSpoken, lang.IsTarget
	if p.CaptionsSource != store.SourceMic && p.CaptionsSource != store.SourceSystem {
		p.CaptionsSource = d.CaptionsSource
	}
	if !spoken(p.CaptionsFrom) {
		p.CaptionsFrom = d.CaptionsFrom
	}
	if !target(p.CaptionsTo) {
		p.CaptionsTo = d.CaptionsTo
	}
	if p.CaptionsTransparency < 0 || p.CaptionsTransparency > 100 {
		p.CaptionsTransparency = d.CaptionsTransparency
	}
	if p.CaptionsDisplay != DisplayInline && p.CaptionsDisplay != DisplaySideBySide {
		p.CaptionsDisplay = d.CaptionsDisplay
	}
	if p.ImportLanguage == lang.Auto || !spoken(p.ImportLanguage) {
		p.ImportLanguage = d.ImportLanguage
	}
	if p.ImportTarget != "" && !target(p.ImportTarget) {
		p.ImportTarget = d.ImportTarget
	}
	if !slices.Contains(highlightEvery, p.HighlightEvery) {
		p.HighlightEvery = d.HighlightEvery
	}
	return p
}

// LoadPrefs reads path; a missing or unreadable file gives the defaults,
// as does a missing setting.
func LoadPrefs(path string) Prefs {
	b, err := os.ReadFile(path)
	if err != nil {
		return DefaultPrefs()
	}
	p := DefaultPrefs()
	if json.Unmarshal(b, &p) != nil {
		return DefaultPrefs()
	}
	return p.normalized()
}

// errInvalidPrefs rejects preferences that normalizing would change.
var errInvalidPrefs = errors.New("views: invalid preferences")

// SavePrefs writes p to path atomically. Invalid preferences are not
// written, so the file never holds silently reset values.
func SavePrefs(path string, p Prefs) error {
	if p.normalized() != p {
		return errInvalidPrefs
	}
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
