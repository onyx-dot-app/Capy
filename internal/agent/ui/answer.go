package ui

import (
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/markdown"
	"github.com/egoist/mygo/ui/markdown/parser"

	"github.com/onyx-dot-app/Capy/internal/agent/ask"
	"github.com/onyx-dot-app/Capy/internal/views/kit"
)

// An answer is Markdown, which Ask's prompt keeps to lists and bold. Its
// citations of turns and, across sessions, the titles of the sessions it
// names become links of Capy's schemes before it shows, which openLink
// opens: turnScheme<session>/<ms> and sessionScheme<session>.
const (
	turnScheme    = "capy-turn:"
	sessionScheme = "capy-session:"
)

// citationRe is a [mm:ss] or [h:mm:ss] the tools gave a turn.
var citationRe = regexp.MustCompile(`\[(?:(\d{1,2}):)?(\d{1,3}):([0-5]\d)\]`)

// linked is an answer with its citations and titles as links, and the
// sessions its citations cite.
type linked struct {
	src   string
	cited []string
}

// linkAnswer links text's citations and titles, outside code. session is
// the thread's session, which a citation cites until a title names
// another; sessions are those whose titles link, for a cross-session
// answer (nil for a session's); turns the openings of sessions' turns
// by time, which a citation's tooltip shows.
func linkAnswer(text, session string, sessions []ask.Session, turns map[string]map[int64]string) linked {
	// The longest title first, so that one inside another links the
	// longer.
	titles := slices.Clone(sessions)
	slices.SortFunc(titles, func(a, b ask.Session) int { return len(b.Title) - len(a.Title) })
	l := &linker{session: session, titles: titles, turns: turns}
	fenced := false
	for line := range strings.Lines(text) {
		if t := strings.TrimLeft(line, " "); strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			fenced = !fenced
			l.b.WriteString(line)
			continue
		}
		if fenced {
			l.b.WriteString(line)
			continue
		}
		// Between backticks is a code span.
		for i, part := range strings.Split(line, "`") {
			if i > 0 {
				l.b.WriteByte('`')
			}
			if i%2 == 1 {
				l.b.WriteString(part)
			} else {
				l.prose(part)
			}
		}
	}
	return linked{src: l.b.String(), cited: l.cited}
}

type linker struct {
	b       strings.Builder
	session string // what a citation cites: the session named last
	titles  []ask.Session
	turns   map[string]map[int64]string
	cited   []string
}

// prose writes text, its titles and citations linked.
func (l *linker) prose(text string) {
	for text != "" {
		cite := citationRe.FindStringSubmatchIndex(text)
		at, title := len(text), ask.Session{}
		if cite != nil {
			at = cite[0]
		}
		for _, t := range l.titles {
			if i := wordIndex(text, t.Title); i >= 0 && i < at {
				at, title = i, t
			}
		}
		l.b.WriteString(text[:at])
		switch {
		case title.ID != "":
			l.b.WriteString("[" + escapeLinkText(title.Title) + "](" + sessionScheme + url.PathEscape(title.ID) + ` "Show Session")`)
			l.session = title.ID
			text = text[at+len(title.Title):]
		case cite != nil && l.session == "": // a citation of no session the answer named
			l.b.WriteString(text[cite[0]:cite[1]])
			text = text[cite[1]:]
		case cite != nil:
			// The time alone, as a chip shows it: "[00:31]" reads 00:31.
			label := strings.Trim(text[cite[0]:cite[1]], "[]")
			ms := citeMs(text, cite)
			tip := l.turns[l.session][ms]
			if tip == "" {
				tip = "Turn at " + label
			}
			l.b.WriteString("[" + label + "](" + turnScheme + url.PathEscape(l.session) + "/" + strconv.FormatInt(ms, 10) + ` "` + escapeLinkTitle(tip) + `")`)
			if !slices.Contains(l.cited, l.session) {
				l.cited = append(l.cited, l.session)
			}
			text = text[cite[1]:]
		default:
			text = ""
		}
	}
}

var (
	linkText  = strings.NewReplacer(`\`, `\\`, `[`, `\[`, `]`, `\]`)
	linkTitle = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\r\n", " ", "\n", " ", "\r", " ")
)

// escapeLinkText is s as a link's text, its brackets its own.
func escapeLinkText(s string) string { return linkText.Replace(s) }

// escapeLinkTitle is s as a link's title, on one line between quotes.
func escapeLinkTitle(s string) string { return linkTitle.Replace(s) }

// wordIndex is the first index of title in s as words of their own, not
// inside others ("Sync" is not in "async"), -1 for none.
func wordIndex(s, title string) int {
	for from := 0; from <= len(s)-len(title); {
		i := strings.Index(s[from:], title)
		if i < 0 {
			return -1
		}
		i += from
		before, _ := utf8.DecodeLastRuneInString(s[:i])
		after, _ := utf8.DecodeRuneInString(s[i+len(title):])
		if !wordRune(before) && !wordRune(after) {
			return i
		}
		_, size := utf8.DecodeRuneInString(s[i:])
		from = i + size
	}
	return -1
}

func wordRune(r rune) bool { return r != utf8.RuneError && (unicode.IsLetter(r) || unicode.IsDigit(r)) }

// citeMs is the time of the citation at cite in s.
func citeMs(s string, cite []int) int64 {
	n := func(i int) int64 {
		if cite[2*i] < 0 {
			return 0
		}
		v, _ := strconv.ParseInt(s[cite[2*i]:cite[2*i+1]], 10, 64)
		return v
	}
	return ((n(1)*60+n(2))*60 + n(3)) * 1000
}

// openLink opens a link of an answer: a session at a cited turn, a
// session a title names, or an address ui.SafeToOpen allows, as a
// ui.Link opens it.
func (p *panel) openLink(c *ui.Context, dest string) {
	if rest, ok := strings.CutPrefix(dest, turnScheme); ok {
		id, at, _ := strings.Cut(rest, "/")
		session, err := url.PathUnescape(id)
		ms, msErr := strconv.ParseInt(at, 10, 64)
		if err == nil && msErr == nil {
			p.openSession(session, ms, true)
		}
		return
	}
	if id, ok := strings.CutPrefix(dest, sessionScheme); ok {
		if session, err := url.PathUnescape(id); err == nil {
			p.openSession(session, 0, false)
		}
		return
	}
	if ui.SafeToOpen(dest) {
		c.OpenURL(dest)
	}
}

// answerTheme is how answers look on a surface of palette pal: 13-point
// text, headings in bold of its size.
func answerTheme(pal kit.Palette) *markdown.Theme {
	h := markdown.HeadingStyle{Size: 1, Weight: 600, Top: 0.75, Bottom: 0.35}
	return &markdown.Theme{FontSize: 13, LineHeight: 20.0 / 13, Text: pal.Label, Muted: pal.Secondary, Link: pal.Link, Strong: pal.Label,
		Code: pal.Label, CodeBackground: pal.Fill, CodeBlockBackground: pal.Fill, CodeBlockBorder: pal.Separator,
		QuoteBar: pal.Separator, QuoteText: pal.Secondary, Rule: pal.Separator, TableBorder: pal.Separator, TableHeader: pal.Fill,
		BlockSpacing: 6.0 / 13, ItemSpacing: 6.0 / 13, Headings: [6]markdown.HeadingStyle{h, h, h, h, h, h}}
}

// plain is an answer without its Markdown, as Copy gives it: a line for
// each paragraph, heading, list item and table row, an item after its
// marker and a row's cells between tabs.
func plain(text string) string {
	d := parser.Parse(text)
	var lines []string
	var walk func(parent int)
	walk = func(parent int) {
		for i := range d.Children(parent) {
			b := d.Block(i)
			switch b.Kind {
			case parser.Paragraph, parser.Heading:
				lines = append(lines, parser.PlainText(d.Inlines(i)))
			case parser.CodeBlock:
				lines = append(lines, strings.TrimRight(d.Lines(i), "\n"))
			case parser.List:
				n := b.Number
				for item := range d.Children(i) {
					marker := "•"
					if b.Ordered {
						marker = strconv.Itoa(n) + "."
						n++
					}
					first := len(lines)
					walk(item)
					if first < len(lines) {
						lines[first] = marker + " " + lines[first]
					}
				}
			case parser.TableRow:
				var cells []string
				for cell := range d.Children(i) {
					cells = append(cells, parser.PlainText(d.Inlines(cell)))
				}
				lines = append(lines, strings.Join(cells, "\t"))
			case parser.Quote, parser.Table:
				walk(i)
			}
		}
	}
	walk(0)
	return strings.Join(lines, "\n")
}
