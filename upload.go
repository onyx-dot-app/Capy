package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"

	"github.com/onyx-dot-app/Capy/internal/audio"
	"github.com/onyx-dot-app/Capy/internal/keychain"
	"github.com/onyx-dot-app/Capy/internal/lang"
	"github.com/onyx-dot-app/Capy/internal/session"
	"github.com/onyx-dot-app/Capy/internal/store"
	"github.com/onyx-dot-app/Capy/internal/store/sqlite"
	"github.com/onyx-dot-app/Capy/internal/zoomai"
)

// uploadTimeout bounds the whole upload, its files one at a time.
const uploadTimeout = time.Hour

type uploadOptions struct {
	files          []string
	name           string // the batch's; "" names it as the app does
	language       string
	target         string // translated to; "" for none
	report, dbPath string
}

// uploadReport is the upload's artifact: its batch and what was saved of
// each file, without IDs, times or hosts, so the same files give the same
// report, and a verdict per check.
type uploadReport struct {
	Batch    string             `json:"batch"`
	Language string             `json:"language"`
	Target   string             `json:"target,omitempty"`
	Files    []uploadReportFile `json:"files"`
	Checks   []reportCheck      `json:"checks"`
	Verdict  string             `json:"verdict"`
}

type uploadReportFile struct {
	Name string `json:"name"`
	// DurationMs is the file's length as its metadata says, 0 when unknown.
	DurationMs int64 `json:"durationMs"`
	// State is the file's in its batch: saved, failed or canceled; Problem
	// why a failed one was not transcribed.
	State    string       `json:"state"`
	Problem  string       `json:"problem,omitempty"`
	Source   string       `json:"source,omitempty"`
	FileName string       `json:"fileName,omitempty"`
	LengthMs int64        `json:"lengthMs"`
	Segments []reportLine `json:"segments"`
	Recap    reportRecap  `json:"recap"`
}

// runUpload imports files as the app's Import does, one at a time, and
// returns the exit code: 0 when every check passed.
func runUpload(o uploadOptions, log *slog.Logger) int {
	r, err := upload(o, log)
	if err != nil {
		fmt.Fprintln(os.Stderr, "capy:", err)
		return 2
	}
	return emit(o.report, r, r.Checks, r.Verdict)
}

func upload(o uploadOptions, log *slog.Logger) (uploadReport, error) {
	if len(o.files) == 0 {
		return uploadReport{}, errors.New("-upload needs the files to upload after its flags")
	}
	if o.language == lang.Auto || !lang.IsSpoken(o.language) {
		return uploadReport{}, fmt.Errorf("-lang-a %q: Speech Summarization needs the language spoken, not auto", o.language)
	}
	if o.target != "" && !lang.IsTarget(o.target) {
		return uploadReport{}, fmt.Errorf("-translate-to %q: not a language Translator writes", o.target)
	}
	key, err := keychain.APIKey()
	if err != nil {
		return uploadReport{}, fmt.Errorf("set $%s or store a key in the app: %w", keychain.EnvKey, err)
	}
	var opts []zoomai.Option
	if u := os.Getenv(baseURLEnv); u != "" {
		opts = append(opts, zoomai.WithBaseURL(u))
	}
	ai, err := zoomai.NewClient(key, opts...)
	if err != nil {
		return uploadReport{}, err
	}
	dbPath := o.dbPath
	if dbPath == "" {
		dir, err := os.MkdirTemp("", "capy-upload-")
		if err != nil {
			return uploadReport{}, err
		}
		defer os.RemoveAll(dir)
		dbPath = filepath.Join(dir, "capy.db")
	}
	ctx, cancel := context.WithTimeout(context.Background(), uploadTimeout)
	defer cancel()
	db, err := sqlite.Open(ctx, dbPath)
	if err != nil {
		return uploadReport{}, err
	}
	defer db.Close()

	b, files, err := newBatch(o)
	if err != nil {
		return uploadReport{}, err
	}
	im := session.NewImporter(session.ImporterOptions{Store: db, Client: func() (*zoomai.Client, error) { return ai, nil },
		Log: log, Parallel: 1})
	if err := im.Start(ctx, b, files); err != nil {
		return uploadReport{}, err
	}
	if err := im.Wait(ctx); err != nil {
		return uploadReport{}, err
	}
	return uploadResult(ctx, db, b)
}

// newBatch is the batch of o's files, each read as the app reads it.
func newBatch(o uploadOptions) (store.Batch, []store.BatchFile, error) {
	b := store.Batch{ID: uuid.NewString(), Name: o.name, Lang: o.language, Target: o.target, CreatedAt: time.Now().UTC()}
	files := make([]store.BatchFile, len(o.files))
	names := make([]string, len(o.files))
	for i, path := range o.files {
		abs, err := filepath.Abs(path)
		if err != nil {
			return store.Batch{}, nil, err
		}
		info, err := os.Stat(abs)
		if err != nil {
			return store.Batch{}, nil, fmt.Errorf("file %d: %w", i+1, errors.Unwrap(err))
		}
		meta, err := audio.Probe(abs)
		if err != nil && !errors.Is(err, audio.ErrUnsupported) {
			return store.Batch{}, nil, err
		}
		names[i] = filepath.Base(abs)
		files[i] = store.BatchFile{ID: uuid.NewString(), Path: abs, Name: names[i], Size: info.Size(),
			Duration: meta.Duration, RecordedAt: meta.RecordedAt}
	}
	if b.Name == "" {
		b.Name = session.BatchName(names, b.CreatedAt)
	}
	return b, files, nil
}

// uploadResult reports batch b as the library keeps it, with its checks.
func uploadResult(ctx context.Context, db store.Store, b store.Batch) (uploadReport, error) {
	got, err := db.GetBatch(ctx, b.ID)
	if err != nil {
		return uploadReport{}, err
	}
	r := uploadReport{Batch: got.Name, Language: got.Lang, Target: got.Target, Files: []uploadReportFile{}}
	for _, bf := range got.Files {
		f := uploadReportFile{Name: bf.Name, DurationMs: bf.Duration.Milliseconds(), State: string(bf.State), Problem: bf.Problem,
			Segments: []reportLine{}}
		inBatch := false
		if bf.SessionID != "" {
			d, err := db.GetSession(ctx, bf.SessionID)
			if err != nil {
				return uploadReport{}, err
			}
			inBatch = d.BatchID == b.ID
			f.Source, f.FileName = string(d.Source), d.FileName
			f.LengthMs = d.EndedAt.Sub(d.StartedAt).Milliseconds()
			f.Recap = reportRecap{Status: string(d.RecapStatus), Title: d.Title, Text: d.Recap}
			for _, s := range d.Segments {
				f.Segments = append(f.Segments, reportLine{Lang: s.Lang, Text: s.Text, Speaker: s.Speaker,
					Translation: s.Translation, TranslationLang: s.TranslationLang})
			}
		}
		r.Files = append(r.Files, f)
		check := func(name string, pass bool, detail string) {
			r.Checks = append(r.Checks, reportCheck{Name: f.Name + ": " + name, Pass: pass, Detail: detail})
		}
		check("transcribed", len(f.Segments) > 0, fmt.Sprintf("%d turns", len(f.Segments)))
		check("saved as its file, in the batch", inBatch && f.Source == string(store.SourceFile) && f.FileName == f.Name &&
			f.Recap.Title == session.FileTitle(f.Name), f.Recap.Title)
		check("summarized", f.Recap.Status == string(store.RecapReady) && f.Recap.Text != "", f.Recap.Status)
		if got.Target != "" {
			translated := 0
			for _, s := range f.Segments {
				if s.Translation != "" && s.TranslationLang == got.Target {
					translated++
				}
			}
			check("translated", len(f.Segments) > 0 && translated == len(f.Segments),
				fmt.Sprintf("%d of %d turns", translated, len(f.Segments)))
		}
	}
	r.Verdict = verdict(r.Checks)
	return r, nil
}
