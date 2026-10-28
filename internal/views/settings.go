package views

import (
	"errors"
	"math"
	"slices"
	"strconv"

	"github.com/egoist/mygo/ui"

	"github.com/onyx-dot-app/Capy/internal/keychain"
	"github.com/onyx-dot-app/Capy/internal/lang"
	"github.com/onyx-dot-app/Capy/internal/profile"
	"github.com/onyx-dot-app/Capy/internal/store"
	"github.com/onyx-dot-app/Capy/internal/views/kit"
)

const apiKeyHelp = "https://zoom.ai"

// keyState is where the API key comes from, as last read.
type keyState int

const (
	keyChecking   keyState = iota // reading the Keychain
	keyNone                       // no key yet
	keyStored                     // in the Keychain
	keyEnv                        // $ZOOM_API_KEY overrides the Keychain
	keyUnreadable                 // the Keychain could not be read
)

// settingsPage is Settings, in place of a session in Capy's window: the
// API key, the user's profile, the captions, Highlights and the add-on's
// section. Leaving it drops what
// it holds, a key typed but not saved included.
type settingsPage struct {
	app *App
	// post runs fn on the main thread, then builds the window anew: its
	// window's.
	post       func(fn func())
	key        string // what is typed; never the stored key
	keyState   keyState
	replacing  bool   // a stored key is being replaced
	busy       string // the key operation in progress, if any
	keyNotice  string // why the last save or delete failed
	confirmDl  bool
	sourceIdx  int
	displayIdx int
	// name and aliases are Profile's fields, read as Settings first shows
	// them: what is typed, saved as it changes.
	name        string
	aliases     []string
	profileRead bool
}

// newSettingsPage returns Settings for window w, reading where the key
// comes from.
func newSettingsPage(w *mainWindow) *settingsPage {
	s := &settingsPage{app: w.app, post: w.post}
	s.changeKey(func() error { return nil }, "Checking the Keychain…", "")
	return s
}

// changeKey runs change, then reads where the key comes from, off the main
// thread: both run /usr/bin/security, which the window must not wait for.
// doing shows meanwhile, and failed when change fails.
func (w *settingsPage) changeKey(change func() error, doing, failed string) {
	w.busy, w.keyNotice = doing, ""
	go func() {
		err := change()
		state := readKeyState()
		w.post(func() { w.keyChanged(state, err, failed) })
	}()
}

// keyChanged shows the outcome of changeKey: a failed change keeps what
// was being done, to try again.
func (w *settingsPage) keyChanged(state keyState, err error, failed string) {
	w.busy, w.keyState, w.keyNotice = "", state, ""
	if err == nil && failed != "" { // a key saved or deleted, not only read
		w.app.highlightsDenied = false // a new key may use Decisions
	}
	if win := w.app.window; win != nil {
		win.reload() // it welcomes a first launch until there is a key
		// Settings, left and opened again meanwhile, read the key before
		// this change was done.
		if now := win.settings; now != nil && now != w {
			now.changeKey(func() error { return nil }, "Checking the Keychain…", "")
		}
	}
	if err != nil {
		w.keyNotice = failed
		return
	}
	w.replacing = false
}

// readKeyState reads where the API key comes from.
func readKeyState() keyState {
	switch _, err := keychain.APIKey(); {
	case keychain.FromEnv():
		return keyEnv
	case err == nil:
		return keyStored
	case errors.Is(err, keychain.ErrNotFound):
		return keyNone
	default:
		return keyUnreadable
	}
}

func (w *settingsPage) view(c *ui.Context) {
	p := &w.app.prefs
	// The Record button's menu changes the source too.
	w.sourceIdx, w.displayIdx = 0, 0
	if p.CaptionsSource == store.SourceSystem {
		w.sourceIdx = 1
	}
	if p.CaptionsDisplay == DisplaySideBySide {
		w.displayIdx = 1
	}
	ui.Scroll(c).Key("settings").Grow(1).Padding(12, 24, 40).Children(func() {
		centered(c).Children(func() {
			ui.Text(c, "Settings").FontSize(26).Bold().Margin(0, 0, 24, 0)
			kit.SectionHeader(c, "Zoom API key")
			w.keyView(c)
			w.profileView(c)
			kit.SectionHeader(c, "Captions")
			kit.Group(c, func() {
				kit.SettingRow(c, "Listen to", func() {
					if segmented(c, &w.sourceIdx, "Listen to", true,
						segment{icon: kit.MicIcon, label: "Microphone"}, segment{icon: kit.LaptopIcon, label: "This Mac"}).Changed() {
						p.CaptionsSource = store.SourceMic
						if w.sourceIdx == 1 {
							p.CaptionsSource = store.SourceSystem
						}
						w.app.savePrefs()
					}
				})
			})
			kit.Footnote(c, "“This Mac” captions any app’s audio and your microphone, so a call in your headphones and your own voice, and needs System Audio Recording and Microphone permission. Let the people you caption know that Zoom AI Services process their speech.")
			kit.Group(c, func() {
				kit.SettingRow(c, "Spoken language", func() {
					if langSelect(c, &p.CaptionsFrom, lang.Spoken(), "Spoken language") {
						w.app.savePrefs()
					}
				})
			}, func() {
				// The check box and the language are independent: the language
				// stays chosen while translation is off.
				ui.Row(c).MinHeight(40).Padding(8, 12).Gap(8).AlignItems(ui.Center).Children(func() {
					on := p.CaptionsTranslate
					if ui.Checkbox(c, &on, "Translate to").Grow(1).Changed() {
						w.app.SetTranslate(on)
					}
					if code := p.CaptionsTo; langSelect(c, &code, lang.Targets(), "Translation language") {
						w.app.setTarget(code)
					}
				})
			})
			kit.Footnote(c, translateHint(*p))
			kit.Group(c, func() {
				kit.SettingRow(c, "Display", func() { w.displaySegmented(c) })
			}, func() {
				kit.SettingRow(c, "Background transparency", func() { w.transparencySlider(c) })
			})
			kit.Footnote(c, "0% is a solid bar; at 100% only the captions show.")
			if w.app.highlightsOn() {
				w.highlightsView(c)
			}
			w.app.ext.Settings(c)
		})
	})
}

// profileView is who the user is: their name and the other names they
// go by, for Highlights and Ask.
func (w *settingsPage) profileView(c *ui.Context) {
	if !w.profileRead {
		w.name, w.aliases, w.profileRead = w.app.profile.Name, slices.Clone(w.app.profile.Aliases), true
	}
	kit.SectionHeader(c, "Profile")
	kit.Group(c, func() {
		kit.SettingRow(c, "Your name", func() {
			if ui.TextInput(c, &w.name).Label("Your name").Width(profileFieldWidth).Changed() {
				w.app.saveProfile(profile.Profile{Name: w.name, Aliases: w.aliases})
			}
		})
	}, func() {
		ui.Row(c).MinHeight(40).Padding(8, 12).Gap(8).AlignItems(ui.Center).Children(func() {
			ui.Column(c).Grow(1).Gap(1).Children(func() {
				ui.Text(c, "Also called").SingleLine()
				ui.Text(c, "Nicknames, your team, your projects.").FontSize(11).TextColor(kit.Colors(c).Secondary)
			})
			if ui.TokenField(c, &w.aliases, nil).Label("Also called").Width(profileFieldWidth).Changed() {
				w.app.saveProfile(profile.Profile{Name: w.name, Aliases: slices.Clone(w.aliases)})
			}
		})
	})
	kit.Footnote(c, "Highlights uses these to mark lines for you, and Ask uses them for “Was I mentioned?”. "+
		"They’re sent only with the requests that need them.")
}

// profileFieldWidth is the width of Profile's fields, one under another.
const profileFieldWidth = 240

// highlightsView is Highlights: whether it marks lines while recording,
// how often it checks, the kinds, how many, and whether a line for the
// user draws attention. The changes apply to a recording from its next
// check.
func (w *settingsPage) highlightsView(c *ui.Context) {
	p := &w.app.prefs
	kit.SectionHeader(c, "Highlights")
	off := !p.Highlights
	labelled := func(label, detail string) {
		ui.Column(c).Grow(1).Basis(0).Gap(1).Children(func() {
			ui.Text(c, label)
			ui.Text(c, detail).FontSize(11).TextColor(kit.Colors(c).Secondary)
		})
	}
	row := func(fn func()) func() {
		return func() { ui.Row(c).MinHeight(40).Padding(8, 12).Gap(12).AlignItems(ui.Center).Children(fn) }
	}
	kit.Group(c, row(func() {
		labelled("Highlight what matters while recording",
			"Capy sends what was said since its last check to Zoom Decisions and marks the lines that matter.")
		if ui.Switch(c, &p.Highlights).Label("Highlight what matters while recording").Changed() {
			w.app.savePrefs()
		}
	}), row(func() {
		labelled("Check every", "Lines that name you are checked right away.")
		var names []string
		for _, n := range highlightEvery {
			names = append(names, everyText(n))
		}
		if chosen, ok := popUp(c, everyText(p.HighlightEvery), names, "Check every"); ok && !off {
			p.HighlightEvery = highlightEvery[slices.Index(names, chosen)]
			w.app.savePrefs()
		}
	}), row(func() {
		ui.Text(c, "Highlight").Grow(1).Basis(0)
		ui.Column(c).Gap(6).AlignItems(ui.Start).Children(func() {
			for _, k := range []struct {
				on    *bool
				label string
			}{
				{&p.HighlightForYou, "Mentions and requests for me"},
				{&p.HighlightDecisions, "Decisions"},
				{&p.HighlightDebates, "Debates"},
				{&p.HighlightActions, "Actions"},
				{&p.HighlightAnnouncements, "Announcements"},
				{&p.HighlightHeadsUps, "Heads-ups"},
			} {
				if ui.Checkbox(c, k.on, k.label).Disabled(off).Changed() {
					w.app.savePrefs()
				}
			}
		})
	}), row(func() {
		labelled("How many", "Fewer marks only what Capy is very sure of.")
		more := 0
		if p.HighlightMore {
			more = 1
		}
		if segmented(c, &more, "How many", true, segment{label: "Fewer", disabled: off}, segment{label: "More", disabled: off}).
			Changed() {
			p.HighlightMore = more == 1
			w.app.savePrefs()
		}
	}), row(func() {
		labelled("When something’s for me", "Glow the caption bar once and show a pill to jump back.")
		if ui.Switch(c, &p.HighlightNudge).Label("When something’s for me").Disabled(off).Changed() {
			w.app.savePrefs()
		}
	}))
	note := "Highlights use your Zoom API key. Capy keeps highlights with the session, on this Mac."
	if w.app.highlightsDenied {
		note = "Your API key can’t use Zoom Decisions, so the last recording wasn’t highlighted. " + note
	}
	kit.Footnote(c, note)
	w.app.ext.HighlightsSettings(c)
}

// everyText is an interval of Check every: "1 minute" or "5 minutes".
func everyText(n int) string {
	if n == 1 {
		return "1 minute"
	}
	return strconv.Itoa(n) + " minutes"
}

// displaySegmented sets how the caption bar lays out translations, which
// an open bar shows at once. Captions without translation have nothing to
// lay out beside them.
func (w *settingsPage) displaySegmented(c *ui.Context) {
	p := &w.app.prefs
	seg := segmented(c, &w.displayIdx, "Display", true,
		segment{icon: kit.InlineIcon, label: "Inline"}, segment{icon: kit.SideBySideIcon, label: "Side by side"}).
		Disabled(!p.CaptionsTranslate)
	if seg.Changed() {
		p.CaptionsDisplay = DisplayInline
		if w.displayIdx == 1 {
			p.CaptionsDisplay = DisplaySideBySide
		}
		w.app.savePrefs()
	}
}

// transparencySlider sets the caption bar's transparency, which an open
// bar shows as it changes.
func (w *settingsPage) transparencySlider(c *ui.Context) {
	p := &w.app.prefs
	v := float64(p.CaptionsTransparency)
	if ui.Slider(c, &v, 0, 100).Label("Background transparency").Width(170).Changed() {
		p.CaptionsTransparency = int(math.Round(v))
		w.app.savePrefs()
	}
	ui.Text(c, strconv.Itoa(p.CaptionsTransparency)+"%").Width(36).TextAlign(ui.End).
		TextColor(kit.Colors(c).Secondary).FontFeatures(kit.Tabular)
}

// translateHint explains the translation setting, and where else it
// turns on and off.
func translateHint(p Prefs) string {
	const toggle = "Turn translation on and off anytime, while recording too, with " + translateShortcutText + " or the caption bar."
	if !p.CaptionsTranslate {
		return "Captions only. " + toggle
	}
	return "Turns spoken in " + langName(p.CaptionsTo) + " stay as they are. " + toggle
}

// keyView shows where the API key comes from, never any of it: the
// Keychain, with Replace… and Delete…, or $ZOOM_API_KEY; else a field to
// paste one.
func (w *settingsPage) keyView(c *ui.Context) {
	p := kit.Colors(c)
	switch {
	case w.keyState == keyChecking:
		kit.Group(c, func() {
			ui.Row(c).MinHeight(40).Padding(8, 12).Gap(8).AlignItems(ui.Center).Children(func() {
				ui.Spinner(c)
				ui.Text(c, w.busy).TextColor(p.Secondary)
			})
		})
		ui.Box(c).Height(20)
	case w.keyState == keyEnv:
		kit.Group(c, func() {
			kit.SettingRow(c, "API key", func() {
				ui.Icon(c, kit.TerminalIcon).Size(15, 15).TextColor(p.Secondary)
				ui.Text(c, "Using $"+keychain.EnvKey).TextColor(p.Secondary).SingleLine()
			})
		})
		kit.Footnote(c, "Set in your environment. It takes precedence over a key in your Keychain; unset it to manage the key here.")
	case w.keyState == keyStored && !w.replacing:
		kit.Group(c, func() {
			kit.SettingRow(c, "API key", func() {
				ui.Row(c).Gap(6).AlignItems(ui.Center).Margin(0, 6, 0, 0).Children(func() {
					kit.CheckBadge(c, p.Success)
					ui.Text(c, "Saved in your Keychain").TextColor(p.Secondary).SingleLine()
				})
				if ui.Button(c, "Replace…").Height(24).Padding(0, 12).Disabled(w.busy != "").Clicked() {
					w.replacing, w.keyNotice = true, ""
				}
				if ui.Button(c, "Delete…").Height(24).Padding(0, 12).Disabled(w.busy != "").Clicked() {
					w.confirmDl = true
				}
			})
		})
		w.keyNote(c, "Need a different key? ")
	default:
		kit.Group(c, func() { w.keyInput(c) })
		w.keyNote(c, "Create a key in the Zoom AI Services console, then paste it here. Capy keeps it in your Keychain. ")
	}
	if ui.AlertDialog(c, &w.confirmDl, "Delete the API key?", "Capy can’t caption until you add a key again.", "Cancel", "Delete") == 1 {
		w.changeKey(keychain.DeleteAPIKey, "Deleting…", "Couldn’t delete the key from the Keychain.")
	}
}

// keyNote is the footnote under the key: what is being done, why it
// failed, or help, then the link to get a key.
func (w *settingsPage) keyNote(c *ui.Context, help string) {
	p := kit.Colors(c)
	failure := w.keyNotice
	if failure == "" && w.keyState == keyUnreadable {
		failure = "The Keychain could not be read."
	}
	if failure != "" && w.busy == "" {
		ui.Row(c).Gap(6).AlignItems(ui.Start).Margin(6, 12, 20).TextColor(p.Danger).Children(func() {
			ui.Icon(c, kit.AlertIcon).Size(14, 14).Margin(1, 0, 0, 0)
			ui.Text(c, failure).FontSize(12).Grow(1).Basis(0)
		})
		return
	}
	if w.busy != "" {
		help = w.busy + " "
	}
	ui.RichText(c).FontSize(12).TextColor(p.Secondary).Margin(6, 12, 20).Children(func() {
		ui.Text(c, help)
		ui.Link(c, "Get a key", apiKeyHelp).TextColor(p.Link)
	})
}

// keyInput takes a key to save, in place of the stored one when replacing.
func (w *settingsPage) keyInput(c *ui.Context) {
	ui.Row(c).MinHeight(40).Padding(8, 12).Gap(8).AlignItems(ui.Center).Children(func() {
		in := ui.TextInput(c, &w.key).Height(24).Password().Placeholder("Paste a key with Scribe, Translator and Summarizer access").Label("API key").Grow(1)
		if w.replacing && (ui.Button(c, "Cancel").Height(24).Padding(0, 12).Disabled(w.busy != "").Clicked() || in.Focused() && c.Shortcut(0, ui.KeyEscape)) && w.busy == "" {
			w.key, w.replacing, w.keyNotice = "", false, ""
		}
		save := ui.PrimaryButton(c, "Save").Height(24).Padding(0, 12).Disabled(w.key == "" || w.busy != "")
		if (in.Submitted() || save.Clicked()) && w.key != "" && w.busy == "" {
			key := w.key
			w.key = ""
			w.changeKey(func() error { return keychain.SetAPIKey(key) }, "Saving…",
				"Couldn’t save the key: it’s empty or malformed, or the Keychain refused it.")
		}
	})
}
