package tools

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Bounds of the write tools.
const (
	maxTitleRunes = 200
	maxRecapBytes = 96 << 10
)

type renameIn struct {
	ID    string `json:"id" jsonschema:"the session's id"`
	Title string `json:"title" jsonschema:"the new title, one line of 1 to 200 characters"`
}

type renameOut struct {
	Title  string `json:"title"`
	EditID string `json:"edit_id"`
}

type recapIn struct {
	ID    string `json:"id" jsonschema:"the session's id"`
	Recap string `json:"recap" jsonschema:"the new recap, Markdown, at most 96 KB"`
}

type editOut struct {
	EditID string `json:"edit_id"`
}

type deleteIn struct {
	ID string `json:"id" jsonschema:"the session's id"`
}

type deleteOut struct {
	MovedTo string `json:"moved_to"`
	EditID  string `json:"edit_id"`
}

func (t *tools) addWrite(s *mcp.Server) {
	add(t, s, &mcp.Tool{Name: "rename_session", Annotations: destructive("Rename a session"),
		Description: "Renames a session. Capy keeps the old title, and the user can undo the rename."},
		t.rename)
	add(t, s, &mcp.Tool{Name: "update_recap", Annotations: destructive("Edit a session's recap"),
		Description: "Replaces a session's recap. Capy keeps the old recap, and the user can undo the edit."},
		t.updateRecap)
	add(t, s, &mcp.Tool{Name: "delete_session", Annotations: destructive("Move a session to Recently Deleted"),
		Description: "Moves a session to Recently Deleted, where the user can restore it for 30 days. A session being recorded cannot be deleted."},
		t.deleteSession)
}

func (t *tools) rename(ctx context.Context, in renameIn) (renameOut, error) {
	if err := validID(in.ID); err != nil {
		return renameOut{}, err
	}
	title := strings.TrimSpace(in.Title)
	if n := utf8.RuneCountInString(title); n == 0 || n > maxTitleRunes {
		return renameOut{}, errors.New("title must be 1 to 200 characters")
	}
	e, err := t.o.Store.RenameSession(ctx, in.ID, title, t.o.Actor, t.o.Origin)
	if err != nil {
		return renameOut{}, toolError(err)
	}
	return renameOut{Title: title, EditID: e.ID}, nil
}

func (t *tools) updateRecap(ctx context.Context, in recapIn) (editOut, error) {
	if err := validID(in.ID); err != nil {
		return editOut{}, err
	}
	if strings.TrimSpace(in.Recap) == "" || len(in.Recap) > maxRecapBytes {
		return editOut{}, errors.New("recap must be 1 byte to 96 KB")
	}
	e, err := t.o.Store.UpdateRecap(ctx, in.ID, in.Recap, t.o.Actor, t.o.Origin)
	if err != nil {
		return editOut{}, toolError(err)
	}
	return editOut{EditID: e.ID}, nil
}

func (t *tools) deleteSession(ctx context.Context, in deleteIn) (deleteOut, error) {
	if err := validID(in.ID); err != nil {
		return deleteOut{}, err
	}
	e, err := t.o.Store.TrashSession(ctx, in.ID, t.o.Actor, t.o.Origin)
	if err != nil {
		return deleteOut{}, toolError(err)
	}
	return deleteOut{MovedTo: "Recently Deleted", EditID: e.ID}, nil
}
