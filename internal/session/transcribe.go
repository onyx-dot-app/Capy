package session

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/onyx-dot-app/Capy/internal/store"
	"github.com/onyx-dot-app/Capy/internal/zoomai"
)

// ErrNotTranscribed is the error of Transcribe when nothing was saved.
// A transcript saved without its summary is not: retry that as any recap.
var ErrNotTranscribed = errors.New("session: file not transcribed")

// FileOptions are how Transcribe transcribes a file and saves it.
type FileOptions struct {
	// Lang is the language spoken in the file; Target the one its
	// transcript is translated to, "" or Lang for none.
	Lang, Target string
	// StartedAt is when its session starts: when the file was recorded,
	// else when it was imported; zero for now.
	StartedAt time.Time
	// BatchID is the batch the file is of, "" for none: its session is
	// created in it, and joins it once its file is saved.
	BatchID string
	// Saved, which may be nil, hears the session's ID as soon as its
	// transcript is saved, before its summary and translations are.
	Saved func(id string)
	// Progress, which may be nil, hears how far Speech Summarization has
	// come.
	Progress func(zoomai.SpeechProgress)
}

// Transcribe sends the audio or video file at path to Speech
// Summarization and saves it as a session of its own, ended, titled by the
// file: its transcript as soon as it arrives, then the transcript's
// translation, if any, and its summary as its recap.
//
// It returns the session's ID once the transcript is saved. Its error then
// reports a summary that failed, saved as a failed recap; without an ID,
// it wraps ErrNotTranscribed. A transcript that arrives as ctx is cancelled
// is saved all the same: it was paid for. A translation that fails is left
// for Retry Translations.
func Transcribe(ctx context.Context, deps Deps, path string, o FileOptions) (string, error) {
	log := deps.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrNotTranscribed, pathless(err))
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrNotTranscribed, pathless(err))
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%w: not a file", ErrNotTranscribed)
	}
	if o.Target == o.Lang {
		o.Target = ""
	}
	if o.StartedAt.IsZero() {
		o.StartedAt = time.Now()
	}
	name := filepath.Base(path)
	s := store.Session{ID: uuid.NewString(), Mode: store.ModeCaptions, Source: store.SourceFile, LangA: o.Lang,
		LangB: o.Lang, StartedAt: o.StartedAt, FileName: name, BatchID: o.BatchID}
	if o.Target != "" {
		s.LangB = o.Target
	}
	if err := s.Validate(); err != nil {
		return "", fmt.Errorf("%w: %w", ErrNotTranscribed, err) // refused once paid for, else
	}

	save := context.WithoutCancel(ctx)
	var (
		kept, created bool
		saveErr       error
		translated    sync.WaitGroup
	)
	// keep saves the transcript, once: as it arrives, before the summary,
	// with the request's tracking ID.
	keep := func(segments []zoomai.SpeechSegment, trackingID string) {
		if kept {
			return
		}
		kept = true
		s.TrackingID = trackingID
		if created, saveErr = saveTranscript(save, deps, s, segments); saveErr != nil {
			return
		}
		if o.Saved != nil {
			o.Saved(s.ID)
		}
		if o.Target != "" {
			translated.Go(func() {
				// Bounded by ctx: a file canceled stops translating, and
				// Retry Translations finishes it.
				if failed, err := RetryTranslations(ctx, deps, s.ID); err != nil || failed > 0 {
					log.Warn("file not translated", "session", s.ID, "failed", failed, "err", err)
				}
			})
		}
	}
	res, err := deps.AI.SummarizeSpeech(ctx, f, info.Size(), name, o.Lang, func(p zoomai.SpeechProgress) {
		if p.Transcript != nil {
			keep(p.Transcript, p.TrackingID)
			p.Transcript = nil
		}
		if o.Progress != nil {
			o.Progress(p)
		}
	})
	if !res.Transcribed {
		log.Warn("file not transcribed", "tracking_id", res.TrackingID, "err", err)
		return "", fmt.Errorf("%w: %w", ErrNotTranscribed, err)
	}
	keep(res.Segments, res.TrackingID)
	translated.Wait()
	switch {
	case !created:
		return "", fmt.Errorf("%w: %w", ErrNotTranscribed, saveErr)
	case saveErr != nil:
		return s.ID, saveErr
	}
	summaryErr := err
	recap := store.Recap{Status: store.RecapReady, Title: FileTitle(name), Text: strings.TrimSpace(res.Summary)}
	if summaryErr != nil {
		recap = store.Recap{Status: store.RecapFailed, Title: FileTitle(name)}
	}
	if err := deps.Store.SetRecap(save, s.ID, recap); err != nil {
		return s.ID, errors.Join(summaryErr, fmt.Errorf("session: save summary: %w", err))
	}
	log.Info("file transcribed", "session", s.ID, "tracking_id", res.TrackingID, "segments", len(res.Segments),
		"summarized", summaryErr == nil, "translated", o.Target != "")
	if summaryErr != nil {
		return s.ID, fmt.Errorf("session: summarize file: %w", summaryErr)
	}
	return s.ID, nil
}

// saveTranscript saves session s, ended as its transcript, segments, ends,
// each turn awaiting its translation when s translates. created reports
// that s was stored, whatever failed after.
func saveTranscript(ctx context.Context, deps Deps, s store.Session, segments []zoomai.SpeechSegment) (created bool, err error) {
	if err := deps.Store.CreateSession(ctx, s); err != nil {
		return false, err
	}
	target := ""
	if s.LangB != s.LangA {
		target = s.LangB
	}
	var lastMs int64
	seq := 0
	for _, seg := range segments {
		// A transcript of text alone is one turn: as long as the file's.
		for _, text := range Chunks(seg.Text, store.MaxSegmentText) {
			if err := deps.Store.AppendSegment(ctx, store.Segment{ID: uuid.NewString(), SessionID: s.ID, Seq: seq,
				Lang: s.LangA, Text: text, Speaker: seg.Speaker, StartMs: seg.StartMs, EndMs: seg.EndMs,
				TranslationLang: target}); err != nil {
				return true, fmt.Errorf("session: save transcript: %w", err)
			}
			seq++
		}
		lastMs = max(lastMs, seg.EndMs)
	}
	if err := deps.Store.EndSession(ctx, s.ID, s.StartedAt.Add(time.Duration(lastMs)*time.Millisecond)); err != nil {
		return true, fmt.Errorf("session: save transcript: %w", err)
	}
	return true, nil
}

// pathless is err without the path a *fs.PathError names: a file's name
// is the user's, and errors end up in logs.
func pathless(err error) error {
	if pe, ok := errors.AsType[*fs.PathError](err); ok {
		return fmt.Errorf("%s: %w", pe.Op, pe.Err)
	}
	return err
}

// FileTitle is the title of a session that transcribed the file named
// name: its name without the extension.
func FileTitle(name string) string {
	if title := strings.TrimSuffix(name, filepath.Ext(name)); strings.TrimSpace(title) != "" {
		return title
	}
	return name
}
