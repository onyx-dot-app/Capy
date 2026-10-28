package ask

import (
	"context"
	"strings"

	"github.com/onyx-dot-app/Capy/internal/agent/threads"
)

// inert is an agent's answer as Markdown that loads nothing when shown:
// an image, or HTML, that a meeting's text asked for could carry the
// user's data to a server as the document opens. Lists, bold and links
// stay.
func inert(text string) string {
	return strings.NewReplacer("![", `!\[`, "<", "&lt;").Replace(text)
}

// Export is the "## Ask" section of session id's Markdown: each question
// of its threads, quoted, with its answer; nothing for a session without
// any.
func (a *Ask) Export(ctx context.Context, id string) ([]string, error) {
	list, err := a.Threads(ctx, id)
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	for _, th := range list {
		d, err := a.o.Threads.GetThread(ctx, th.ID)
		if err != nil {
			return nil, err
		}
		for _, m := range d.Messages {
			switch m.Role {
			case threads.RoleUser:
				b.WriteString("\n> ")
				b.WriteString(strings.ReplaceAll(m.Text, "\n", "\n> "))
				b.WriteString("\n")
				if m.Status == threads.StatusUnanswered {
					b.WriteString("\n_Not answered._\n")
				}
			case threads.RoleAgent:
				b.WriteString("\n")
				b.WriteString(inert(m.Text))
				b.WriteString("\n")
				if m.Status == threads.StatusIncomplete {
					b.WriteString("\n_The answer stopped early._\n")
				}
			}
		}
	}
	if b.Len() == 0 {
		return nil, nil
	}
	return []string{"## Ask\n" + b.String()}, nil
}
