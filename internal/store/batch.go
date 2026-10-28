package store

import (
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Batch is an import: audio or video files transcribed together, each into
// a session of its own, in one spoken language, translated or not.
type Batch struct {
	ID   string
	Name string
	// Lang is the language spoken in its files; Target the one their
	// transcripts are translated to, "" for none.
	Lang, Target string
	CreatedAt    time.Time
}

// FileState is where a file of a batch is.
type FileState string

const (
	// FileWaiting is a file to transcribe, or being transcribed.
	FileWaiting FileState = "waiting"
	// FileSaved is a file saved as a session: its SessionID.
	FileSaved FileState = "saved"
	// FileFailed is a file that could not be transcribed: its Problem says
	// why.
	FileFailed FileState = "failed"
	// FileCanceled is a file the user stopped before it was saved.
	FileCanceled FileState = "canceled"
)

// BatchFile is a file of a batch.
type BatchFile struct {
	ID, BatchID string
	// Seq is its place in the batch, in the order the files were chosen,
	// from 0; CreateBatch numbers them.
	Seq int
	// Path is where the file is, kept to transcribe it, after a restart
	// too, until it is saved: then it is "".
	Path string
	// Name is the file's name, without its folders.
	Name string
	Size int64
	// Duration is how long its audio lasts and RecordedAt when it was
	// recorded, from its metadata; zero when it has none.
	Duration   time.Duration
	RecordedAt time.Time
	State      FileState
	// Problem is why a failed file was not transcribed: a code of the
	// importer's, which the store gives no meaning.
	Problem string
	// Title is the name the user gave the file before it was saved; its
	// session takes it as its rename.
	Title string
	// SessionID is the session a saved file is, and Session that session
	// as listed: zero for a file not saved.
	SessionID string
	Session   SessionSummary
	// Recap is the saved session's recap, read by GetBatch only.
	Recap string
}

// BatchSummary is a batch with its files, in order.
type BatchSummary struct {
	Batch
	Files []BatchFile
}

// Batch bounds, enforced by the Validate methods.
const (
	MaxBatchFiles = 500
	MaxPathLen    = 4 << 10 // bytes: macOS allows 1024 UTF-16 units
)

// Validate checks b against the contract.
func (b Batch) Validate() error {
	if err := ValidateID("batch id", b.ID); err != nil {
		return err
	}
	if err := ValidateBatchName(b.Name); err != nil {
		return err
	}
	if err := validateLang("batch lang", b.Lang, true); err != nil {
		return err
	}
	if err := validateLang("batch target", b.Target, false); err != nil {
		return err
	}
	if b.CreatedAt.IsZero() {
		return invalid("created at", "is required")
	}
	return nil
}

// ValidateBatchName checks a batch's name: as a title, and not blank.
func ValidateBatchName(name string) error {
	if strings.TrimSpace(name) == "" {
		return invalid("batch name", "is required")
	}
	return ValidateTitle(name)
}

// Validate checks f, a file of a new batch, against the contract.
func (f BatchFile) Validate() error {
	if err := ValidateID("file id", f.ID); err != nil {
		return err
	}
	if err := ValidateFileName(f.Name); err != nil {
		return err
	}
	switch {
	case f.Path == "" || !filepath.IsAbs(f.Path):
		return invalid("file path", "must be absolute")
	case len(f.Path) > MaxPathLen:
		return invalid("file path", "is too long")
	case !utf8.ValidString(f.Path) || strings.ContainsFunc(f.Path, unsafeInName):
		return invalid("file path", "must be printable")
	case f.Size < 0 || f.Duration < 0:
		return invalid("file size", "must not be negative")
	}
	return ValidateTitle(f.Title)
}

// ValidateFileState checks the arguments of SetFileState: a state a file
// not saved may take, with a problem for a failed one only.
func ValidateFileState(fileID string, state FileState, problem string) error {
	if err := ValidateID("file id", fileID); err != nil {
		return err
	}
	switch state {
	case FileWaiting, FileCanceled:
		if problem != "" {
			return invalid("problem", "is for a failed file only")
		}
		return nil
	case FileFailed:
		return ValidateTag("problem", problem, true)
	}
	return invalid("file state", "is unknown")
}

// ValidateBatch checks the arguments of CreateBatch.
func ValidateBatch(b Batch, files []BatchFile) error {
	if err := b.Validate(); err != nil {
		return err
	}
	if len(files) == 0 || len(files) > MaxBatchFiles {
		return invalid("batch files", "must number 1 to "+strconv.Itoa(MaxBatchFiles))
	}
	for _, f := range files {
		if err := f.Validate(); err != nil {
			return err
		}
	}
	return nil
}
