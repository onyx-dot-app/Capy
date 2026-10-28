package session

import (
	"strings"

	"github.com/onyx-dot-app/Capy/internal/lang"
	"github.com/onyx-dot-app/Capy/internal/store"
)

// Markdown renders a saved session as a document: title, when and how it
// was recorded, the recap, its highlights, and the transcript with
// translations, then sections, each a Markdown section with its heading.
func Markdown(d store.SessionDetail, sections ...string) string {
	var b strings.Builder
	title := d.Title
	if title == "" {
		title = "Capy session"
	}
	b.WriteString("# " + title + "\n\n")
	mode := "Captions"
	switch {
	case d.Source == store.SourceFile:
		mode = d.FileName
	case d.Mode == store.ModeConversation:
		mode = "Conversation"
	}
	var names []string
	for _, code := range SpokenLanguages(d) {
		names = append(names, name(code))
	}
	langs := strings.Join(names, ", ")
	if d.Translated {
		langs += " · " + name(d.LangB)
	}
	b.WriteString("*" + mode + " · " + langs + " · " + d.StartedAt.Local().Format("2006-01-02 15:04") + "*\n\n")
	if d.Recap != "" {
		b.WriteString("## Recap\n\n" + strings.TrimSpace(d.Recap) + "\n\n")
	}
	writeHighlights(&b, d)
	b.WriteString("## Transcript\n\n")
	named := ManySpeakers(d.Segments, SegmentSpeaker)
	for _, seg := range d.Segments {
		if seg.Lang == store.GapLang {
			b.WriteString("> *(connection lost)*\n\n")
			continue
		}
		b.WriteString("- ")
		if named && seg.Speaker != "" {
			b.WriteString("**" + seg.Speaker + ":** ")
		}
		b.WriteString(seg.Text + "\n")
		if seg.Translation != "" {
			b.WriteString("  - *" + seg.Translation + "*\n")
		}
	}
	for _, sec := range sections {
		b.WriteString("\n" + strings.TrimSpace(sec) + "\n")
	}
	return b.String()
}

// writeHighlights lists the highlights not dismissed, nor folded into
// another, in time order: "- **Asked you** [12:21] what was said", its
// polish's sentence in place of what was said once polished, and a
// debate's topic with where it stands.
func writeHighlights(b *strings.Builder, d store.SessionDetail) {
	said := map[int]store.Segment{}
	for _, seg := range d.Segments {
		said[seg.Seq] = seg
	}
	var lines []string
	for _, h := range d.Highlights {
		seg, ok := said[h.Seq]
		if h.Dismissed || h.FoldedAmong(d.Highlights) || !ok || seg.Text == "" {
			continue
		}
		text := seg.Text
		if s := h.Sentence(); s != "" {
			text = agentText.Replace(s)
		}
		if h.Polish != nil && h.Polish.Debate != nil {
			text += " (" + string(h.Polish.Debate.Status) + ")"
		}
		lines = append(lines, "- **"+h.ShownKind().Label()+"** ["+stamp(seg.StartMs)+"] "+text+"\n")
	}
	if len(lines) > 0 {
		b.WriteString("## Highlights\n\n" + strings.Join(lines, "") + "\n")
	}
}

// agentText escapes what Markdown would make of an agent's text, which a
// transcript may steer: an image that loads, a link, a tag.
var agentText = strings.NewReplacer(`\`, `\\`, "`", "\\`", "*", `\*`, "_", `\_`, "[", `\[`, "]", `\]`,
	"(", `\(`, ")", `\)`, "!", `\!`, "<", `\<`, ">", `\>`)

func name(code string) string {
	if l, ok := lang.Lookup(code); ok {
		return l.Name
	}
	return code
}
