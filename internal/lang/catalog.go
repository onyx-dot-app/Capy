// Package lang holds the languages Capy supports.
package lang

import (
	"slices"
	"strings"
)

const english = "en-US"

// Auto is the spoken language that has Scribe detect each turn's language
// among the Detectable ones.
const Auto = "auto"

// Undetermined is the BCP-47 code of a turn whose language Scribe did not
// name.
const Undetermined = "und"

// auto is Auto's catalog entry.
var auto = Language{Auto, "Auto-detect"}

// Language is a BCP-47 code with its native name.
type Language struct {
	Code string
	Name string
}

// targets is the Zoom Translator set, English first. Immutable.
var targets = []Language{
	{"en-US", "English"},
	{"zh-CN", "中文 (简体)"},
	{"zh-TW", "中文 (繁體)"},
	{"ja-JP", "日本語"},
	{"ko-KR", "한국어"},
	{"es-ES", "Español"},
	{"fr-FR", "Français"},
	{"de-DE", "Deutsch"},
	{"pt-BR", "Português (Brasil)"},
	{"it-IT", "Italiano"},
}

// scribe is the set of codes Zoom Scribe can transcribe. Immutable.
var scribe = map[string]bool{
	"en-US": true, "zh-CN": true, "ja-JP": true, "es-ES": true,
	"it-IT": true, "fr-FR": true, "de-DE": true, "pt-BR": true,
}

// summarizer is the set of catalog codes Zoom Summarizer writes in. Immutable.
var summarizer = map[string]bool{
	"en-US": true, "zh-CN": true, "ja-JP": true, "es-ES": true, "fr-FR": true,
	"de-DE": true, "pt-BR": true, "it-IT": true,
}

// IsSpoken reports whether code is usable as a spoken language, Auto
// included.
func IsSpoken(code string) bool { return code == Auto || scribe[code] }

// IsTarget reports whether code is usable as a translation target.
func IsTarget(code string) bool {
	return slices.ContainsFunc(targets, func(l Language) bool { return l.Code == code })
}

// Summarizable reports whether Summarizer can write a recap in code.
func Summarizable(code string) bool { return summarizer[code] }

// Spoken returns the choices of spoken language: Auto, then the
// Detectable languages.
func Spoken() []Language {
	return append([]Language{auto}, Detectable()...)
}

// Detectable returns the languages Scribe can transcribe and Translator can
// translate, English first: those Auto detects among.
func Detectable() []Language {
	var out []Language
	for _, l := range targets {
		if scribe[l.Code] {
			out = append(out, l)
		}
	}
	return out
}

// FromDetected returns the catalog code of a language Scribe detected. Scribe
// names it by its ISO 639-1 code ("en", "zh"), which matches the first
// catalog language of that primary subtag; a full catalog code matches
// itself.
func FromDetected(code string) (string, bool) {
	primary, _, _ := strings.Cut(code, "-")
	for _, l := range targets {
		if strings.EqualFold(l.Code, code) {
			return l.Code, true
		}
	}
	for _, l := range targets {
		if p, _, _ := strings.Cut(l.Code, "-"); primary != "" && strings.EqualFold(p, primary) {
			return l.Code, true
		}
	}
	return "", false
}

// Targets returns the languages usable as a translation target.
func Targets() []Language {
	return slices.Clone(targets)
}

// Lookup returns the catalog entry for code, Auto included.
func Lookup(code string) (Language, bool) {
	if code == Auto {
		return auto, true
	}
	for _, l := range targets {
		if l.Code == code {
			return l, true
		}
	}
	return Language{}, false
}

// TranslationSteps returns the Translator calls needed to go from one language
// to another, as {source, target} pairs. Translator only translates to or from
// English, so two non-English languages are bridged through en-US. It returns
// nil when from == to.
func TranslationSteps(from, to string) [][2]string {
	switch {
	case from == to:
		return nil
	case from == english || to == english:
		return [][2]string{{from, to}}
	default:
		return [][2]string{{from, english}, {english, to}}
	}
}
