package session

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/onyx-dot-app/Capy/internal/store"
	"github.com/onyx-dot-app/Capy/internal/zoomai"
)

// Problems a failed file is kept with: why it was not transcribed.
const (
	ProblemMissing    = "missing"    // the file is no longer where it was
	ProblemUnreadable = "unreadable" // it could not be read
	ProblemType       = "type"       // Zoom does not take its type
	ProblemTooLarge   = "too-large"  // over zoomai.MaxSpeechFileBytes
	ProblemKey        = "key"        // the API key is missing or refused
	ProblemTimeout    = "timeout"    // Zoom took longer than the file may
	ProblemBusy       = "busy"       // Zoom kept limiting the rate
	ProblemTranscribe = "transcribe" // Zoom could not transcribe it
	ProblemUpload     = "upload"     // it could not be sent
)

// Pause is why an Importer starts no file now.
type Pause int

const (
	// Running is no pause.
	Running Pause = iota
	// PausedOffline waits for the connection to come back: a file could
	// not reach Zoom. The importer tries again on its own.
	PausedOffline
	// PausedRecording waits for a live recording to reconnect, which needs
	// the connection more.
	PausedRecording
)

// Importer defaults.
const (
	DefaultParallel    = 2
	DefaultFileTimeout = 30 * time.Minute
	defaultOfflineWait = 5 * time.Second
	maxOfflineWait     = time.Minute
)

// ImporterOptions are what an Importer needs.
type ImporterOptions struct {
	Store store.Store
	// Client makes the Zoom client a file is sent with, reading the API
	// key: it is called off the main thread.
	Client func() (*zoomai.Client, error)
	Log    *slog.Logger
	// Parallel is how many files are transcribed at a time, FileTimeout
	// how long one may take; zero for the defaults.
	Parallel    int
	FileTimeout time.Duration
	// OfflineWait is how long the importer waits to try again after a
	// file could not reach Zoom, twice as long each time it still cannot,
	// up to a minute; zero for 5 seconds.
	OfflineWait time.Duration
	// Changed hears that a file moved on, from any goroutine; Finished
	// that batch batchID has no file left to transcribe, once its last
	// file's state is stored. Either may be nil. Both are called without
	// the importer's lock, and may call it.
	Changed  func()
	Finished func(batchID string)
}

// Importer transcribes the files of batches, Parallel at a time, each into
// a session of its own. Its queue is the store's files waiting: Resume
// picks a batch up where it was left, after a restart too. It is safe for
// concurrent use.
type Importer struct {
	o ImporterOptions

	mu      sync.Mutex
	queue   []job            // in the order they are taken
	running map[string]*work // by file ID
	offline bool             // waiting for the connection: retry is set
	retry   *time.Timer
	tries   int  // how many times in a row a file could not reach Zoom
	held    bool // a recording reconnects
	stopped bool // the app quits: the files stay waiting
	idle    chan struct{}
}

// job is a file to transcribe, of its batch.
type job struct {
	batch store.Batch
	file  store.BatchFile
}

// work is a file being transcribed.
type work struct {
	batchID  string
	cancel   context.CancelFunc
	progress zoomai.SpeechProgress
	canceled bool // by the user
}

// NewImporter returns an importer with nothing to transcribe.
func NewImporter(o ImporterOptions) *Importer {
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.Parallel <= 0 {
		o.Parallel = DefaultParallel
	}
	if o.FileTimeout <= 0 {
		o.FileTimeout = DefaultFileTimeout
	}
	if o.OfflineWait <= 0 {
		o.OfflineWait = defaultOfflineWait
	}
	idle := make(chan struct{})
	close(idle)
	return &Importer{o: o, running: make(map[string]*work), idle: idle}
}

// Start stores batch b of files, in order, and transcribes them.
func (im *Importer) Start(ctx context.Context, b store.Batch, files []store.BatchFile) error {
	if err := im.o.Store.CreateBatch(ctx, b, files); err != nil {
		return err
	}
	jobs := make([]job, len(files))
	for i, f := range files {
		f.BatchID, f.Seq, f.State = b.ID, i, store.FileWaiting
		jobs[i] = job{batch: b, file: f}
	}
	im.enqueue(jobs...)
	return nil
}

// Resume transcribes the files of every batch left waiting, as by a quit,
// and returns how many.
func (im *Importer) Resume(ctx context.Context) (int, error) {
	batches, err := im.o.Store.ListBatches(ctx)
	if err != nil {
		return 0, err
	}
	var jobs []job
	for _, b := range slices.Backward(batches) { // the oldest first
		for _, f := range b.Files {
			if f.State == store.FileWaiting {
				jobs = append(jobs, job{batch: b.Batch, file: f})
			}
		}
	}
	return im.enqueue(jobs...), nil
}

// Retry transcribes file fileID of batch batchID again, after it failed
// or was canceled.
func (im *Importer) Retry(ctx context.Context, batchID, fileID string) error {
	return im.retryWhere(ctx, batchID, func(f store.BatchFile) bool {
		return f.ID == fileID && (f.State == store.FileFailed || f.State == store.FileCanceled)
	})
}

// RetryFailed transcribes the files of batch batchID that failed again.
func (im *Importer) RetryFailed(ctx context.Context, batchID string) error {
	return im.retryWhere(ctx, batchID, func(f store.BatchFile) bool { return f.State == store.FileFailed })
}

func (im *Importer) retryWhere(ctx context.Context, batchID string, retry func(store.BatchFile) bool) error {
	b, err := im.o.Store.GetBatch(ctx, batchID)
	if err != nil {
		return err
	}
	var jobs []job
	for _, f := range b.Files {
		if !retry(f) {
			continue
		}
		if err := im.o.Store.SetFileState(ctx, f.ID, store.FileWaiting, ""); err != nil {
			return err
		}
		f.State, f.Problem = store.FileWaiting, ""
		jobs = append(jobs, job{batch: b.Batch, file: f})
	}
	im.enqueue(jobs...)
	return nil
}

// enqueue queues jobs not queued or running yet, and returns how many.
func (im *Importer) enqueue(jobs ...job) int {
	im.mu.Lock()
	wasIdle := im.isIdle()
	n := 0
	for _, j := range jobs {
		if im.has(j.file.ID) {
			continue
		}
		im.queue = append(im.queue, j)
		n++
	}
	if n > 0 && wasIdle {
		im.idle = make(chan struct{})
	}
	im.pump()
	im.mu.Unlock()
	im.changed()
	return n
}

// has reports a file queued or running. Lock held.
func (im *Importer) has(fileID string) bool {
	_, running := im.running[fileID]
	return running || slices.ContainsFunc(im.queue, func(j job) bool { return j.file.ID == fileID })
}

// isIdle reports nothing queued nor running. Lock held.
func (im *Importer) isIdle() bool { return len(im.queue) == 0 && len(im.running) == 0 }

// settle wakes Wait once nothing is queued nor running. Lock held.
func (im *Importer) settle() {
	if !im.isIdle() {
		return
	}
	select {
	case <-im.idle:
	default:
		close(im.idle)
	}
}

// pump starts the files queued that may run. Lock held.
func (im *Importer) pump() {
	for len(im.running) < im.o.Parallel && len(im.queue) > 0 && !im.offline && !im.held && !im.stopped {
		j := im.queue[0]
		im.queue = im.queue[1:]
		ctx, cancel := context.WithTimeout(context.Background(), im.o.FileTimeout)
		w := &work{batchID: j.batch.ID, cancel: cancel}
		im.running[j.file.ID] = w
		go im.run(ctx, j, w)
	}
}

// run transcribes j.
func (im *Importer) run(ctx context.Context, j job, w *work) {
	defer w.cancel()
	ai, err := im.o.Client()
	id := ""
	if err != nil {
		err = fmt.Errorf("%w: %w", errNoClient, err)
	} else {
		started := j.file.RecordedAt
		if started.IsZero() {
			started = j.batch.CreatedAt
		}
		id, err = Transcribe(ctx, Deps{AI: ai, Store: im.o.Store, Log: im.o.Log}, j.file.Path, FileOptions{
			Lang: j.batch.Lang, Target: j.batch.Target, StartedAt: started, BatchID: j.batch.ID,
			Saved:    func(sid string) { im.saved(j.file.ID, sid) },
			Progress: func(p zoomai.SpeechProgress) { im.progressed(j.file.ID, p) },
		})
	}
	im.done(j, w, id, err)
}

// saved records that file fileID was saved as session sid. A file taken
// out of its batch meanwhile is no more: its session is left on its own.
func (im *Importer) saved(fileID, sid string) {
	if err := im.o.Store.SaveFile(context.Background(), fileID, sid); err != nil && !errors.Is(err, store.ErrNotFound) {
		im.o.Log.Error("save file", "err", err)
	}
	im.changed()
}

// progressed keeps how far file fileID has come. A new stage is a change;
// the percent uploaded is read when shown.
func (im *Importer) progressed(fileID string, p zoomai.SpeechProgress) {
	im.mu.Lock()
	w := im.running[fileID]
	moved := w != nil && w.progress.Stage != p.Stage
	if w != nil {
		w.progress = p
	}
	im.mu.Unlock()
	if moved {
		im.changed()
	}
}

// done records what came of j: saved as session id, if not "", or err.
// A file that could not reach Zoom waits for the connection, first in the
// queue; one stopped by a quit stays waiting.
func (im *Importer) done(j job, w *work, id string, err error) {
	ctx := context.Background()
	im.mu.Lock()
	delete(im.running, j.file.ID)
	stored := true
	switch {
	case im.stopped:
	case id != "":
		im.tries = 0
	case w.canceled:
		stored = im.setState(ctx, j.file.ID, store.FileCanceled, "")
	case errors.Is(err, zoomai.ErrConnection):
		im.queue = append([]job{j}, im.queue...)
		im.goOffline()
	default:
		im.o.Log.Warn("file failed", "problem", problemOf(err), "err", err)
		stored = im.setState(ctx, j.file.ID, store.FileFailed, problemOf(err))
	}
	im.pump()
	finished := stored && !im.stopped && !im.busy(j.batch.ID)
	im.settle()
	im.mu.Unlock()
	im.changed()
	if finished && im.o.Finished != nil {
		im.o.Finished(j.batch.ID)
	}
}

// setState stores a file's state and reports whether it was stored: a file
// taken out of its batch meanwhile is no more. Lock held, so that a retry
// cannot store its own first.
func (im *Importer) setState(ctx context.Context, fileID string, state store.FileState, problem string) bool {
	err := im.o.Store.SetFileState(ctx, fileID, state, problem)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		im.o.Log.Error("set file state", "err", err)
	}
	return err == nil
}

// goOffline waits before trying the queue again, twice as long as the last
// time while the files still cannot reach Zoom. Lock held.
func (im *Importer) goOffline() {
	if im.offline {
		return
	}
	im.offline = true
	wait := min(im.o.OfflineWait<<min(im.tries, 8), maxOfflineWait)
	im.tries++
	im.o.Log.Info("import paused: offline", "wait", wait)
	im.retry = time.AfterFunc(wait, func() {
		im.mu.Lock()
		im.offline = false
		im.pump()
		im.mu.Unlock()
		im.changed()
	})
}

// busy reports a file of batch batchID queued or running, of any batch
// for "". Lock held.
func (im *Importer) busy(batchID string) bool {
	for _, w := range im.running {
		if batchID == "" || w.batchID == batchID {
			return true
		}
	}
	return slices.ContainsFunc(im.queue, func(j job) bool { return batchID == "" || j.batch.ID == batchID })
}

// Cancel stops file fileID: a file queued is not sent, one under way
// stops unless its transcript arrived.
func (im *Importer) Cancel(fileID string) {
	im.cancelWhere(func(id, _ string) bool { return id == fileID })
}

// CancelBatch stops every file of batch batchID, as Cancel does.
func (im *Importer) CancelBatch(batchID string) {
	im.cancelWhere(func(_, b string) bool { return b == batchID })
}

func (im *Importer) cancelWhere(match func(fileID, batchID string) bool) {
	ctx := context.Background()
	im.mu.Lock()
	var finished []string
	kept := im.queue[:0]
	for _, j := range im.queue {
		if !match(j.file.ID, j.batch.ID) {
			kept = append(kept, j)
			continue
		}
		if im.setState(ctx, j.file.ID, store.FileCanceled, "") && !slices.Contains(finished, j.batch.ID) {
			finished = append(finished, j.batch.ID)
		}
	}
	im.queue = kept
	for id, w := range im.running {
		if match(id, w.batchID) {
			w.canceled = true
			w.cancel()
		}
	}
	// A batch with files running finishes as they stop.
	finished = slices.DeleteFunc(finished, im.busy)
	im.settle()
	im.mu.Unlock()
	im.changed()
	for _, id := range finished {
		if im.o.Finished != nil {
			im.o.Finished(id)
		}
	}
}

// Hold holds the files not started while on is true, as while a recording
// reconnects.
func (im *Importer) Hold(on bool) {
	im.mu.Lock()
	if im.held == on {
		im.mu.Unlock()
		return
	}
	im.held = on
	im.pump()
	im.mu.Unlock()
	im.changed()
}

// Stop stops every file under way as the app quits: they stay waiting, for
// Resume at the next launch.
func (im *Importer) Stop() {
	im.mu.Lock()
	im.stopped, im.queue = true, nil
	for _, w := range im.running {
		w.cancel()
	}
	if im.retry != nil {
		im.retry.Stop()
	}
	im.settle()
	im.mu.Unlock()
}

// Wait waits until nothing is queued or running, or ctx ends.
func (im *Importer) Wait(ctx context.Context) error {
	im.mu.Lock()
	idle := im.idle
	im.mu.Unlock()
	select {
	case <-idle:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Paused says why no file starts now, Running when they may.
func (im *Importer) Paused() Pause {
	im.mu.Lock()
	defer im.mu.Unlock()
	switch {
	case im.offline:
		return PausedOffline
	case im.held:
		return PausedRecording
	}
	return Running
}

// Busy reports a file of batch batchID queued or under way; of any batch
// for "".
func (im *Importer) Busy(batchID string) bool {
	im.mu.Lock()
	defer im.mu.Unlock()
	return im.busy(batchID)
}

// Progress is how far file fileID has come, while it is under way.
func (im *Importer) Progress(fileID string) (zoomai.SpeechProgress, bool) {
	im.mu.Lock()
	defer im.mu.Unlock()
	w, ok := im.running[fileID]
	if !ok {
		return zoomai.SpeechProgress{}, false
	}
	return w.progress, true
}

// Unsent is how many files of batch batchID, of any batch for "", have not
// been sent whole: queued, or uploading.
func (im *Importer) Unsent(batchID string) int {
	im.mu.Lock()
	defer im.mu.Unlock()
	n := 0
	for _, j := range im.queue {
		if batchID == "" || j.batch.ID == batchID {
			n++
		}
	}
	for _, w := range im.running {
		if (batchID == "" || w.batchID == batchID) && w.progress.Stage == zoomai.StageUploading {
			n++
		}
	}
	return n
}

func (im *Importer) changed() {
	if im.o.Changed != nil {
		im.o.Changed()
	}
}

// problemOf is why err kept a file from being transcribed.
func problemOf(err error) string {
	var api *zoomai.APIError
	var speech *zoomai.SpeechError
	var limited *zoomai.RateLimitedError
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return ProblemMissing
	case errors.Is(err, fs.ErrPermission):
		return ProblemUnreadable
	case errors.Is(err, zoomai.ErrUnsupportedFile):
		return ProblemType
	case errors.Is(err, zoomai.ErrFileTooLarge):
		return ProblemTooLarge
	case errors.Is(err, zoomai.ErrUnauthorized), errors.Is(err, errNoClient):
		return ProblemKey
	case errors.Is(err, context.DeadlineExceeded):
		return ProblemTimeout
	case errors.As(err, &limited):
		return ProblemBusy
	case errors.As(err, &speech), errors.As(err, &api):
		return ProblemTranscribe
	}
	return ProblemUpload
}

// errNoClient is a file never sent: there was no Zoom client to send it
// with, for want of an API key.
var errNoClient = errors.New("session: no Zoom client")

// BatchName is the name a batch of the files named names, imported at at,
// takes until the user names it: the start their titles share, else the
// day it was imported; one file's title for one file.
func BatchName(names []string, at time.Time) string {
	if len(names) == 1 {
		return FileTitle(names[0])
	}
	shared := []rune(FileTitle(names[0]))
	for _, name := range names[1:] {
		title := []rune(FileTitle(name))
		n := 0
		for n < len(shared) && n < len(title) && shared[n] == title[n] {
			n++
		}
		shared = shared[:n]
	}
	if name := strings.TrimRight(string(shared), " -_.,()0123456789"); utf8.RuneCountInString(name) >= 3 {
		return name
	}
	return "Imported " + at.Local().Format("Jan 2")
}
