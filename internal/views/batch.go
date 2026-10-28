package views

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/egoist/mygo"

	"github.com/onyx-dot-app/Capy/internal/session"
	"github.com/onyx-dot-app/Capy/internal/store"
	"github.com/onyx-dot-app/Capy/internal/zoomai"
)

// batchNotification prefixes the ID of the notification that a batch is
// done: clicking it opens the batch.
const batchNotification = "capy-batch:"

// newImporter is the app's importer: what it changes shows in Capy's
// window and the menu, and a batch done notifies.
func (a *App) newImporter() *session.Importer {
	return session.NewImporter(session.ImporterOptions{
		Store: a.store, Client: a.client, Log: a.log,
		Changed:  func() { a.onMain(a.importsChanged) },
		Finished: a.batchFinished,
	})
}

// importsChanged shows where the files being imported are: the library
// lists the sessions saved, the menu the batches under way.
func (a *App) importsChanged() { a.libraryChanged() }

// batchFinished tells the user batch id is done, unless they canceled a
// file of it: they were there. It reads the batch off the main thread.
func (a *App) batchFinished(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	b, err := a.store.GetBatch(ctx, id)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			a.log.Warn("read batch done", "err", err)
		}
		return
	}
	title, body, ok := finishedText(b)
	if !ok {
		return
	}
	a.onMain(func() { a.notify(batchNotification+id, title, body) })
}

// showNotification shows notification id, whose click opens what id names.
func (a *App) showNotification(id, title, body string) {
	n := mygo.NewNotification(mygo.NotificationOptions{ID: id, Title: title, Body: body})
	if err := n.Show(); err != nil && !errors.Is(err, mygo.ErrNotificationsDenied) {
		a.log.Warn("notification", "err", err)
	}
}

// finishedText is the notification that batch b is done; ok is false when
// a file of it was canceled.
func finishedText(b store.BatchSummary) (title, body string, ok bool) {
	var saved, failed []store.BatchFile
	for _, f := range b.Files {
		switch f.State {
		case store.FileSaved:
			saved = append(saved, f)
		case store.FileFailed:
			failed = append(failed, f)
		case store.FileCanceled:
			return "", "", false
		}
	}
	if len(failed) == 0 {
		body = strconv.Itoa(len(saved)) + " sessions transcribed and summarized."
		if len(saved) == 1 {
			body = "1 session transcribed and summarized."
		}
		return b.Name + " is ready", body, true
	}
	body = strconv.Itoa(len(saved)) + " of " + strconv.Itoa(len(b.Files)) + " are ready. "
	if len(failed) == 1 {
		body += fileTitle(failed[0]) + " couldn’t be transcribed."
	} else {
		body += strconv.Itoa(len(failed)) + " files couldn’t be transcribed."
	}
	return b.Name + " finished with a problem", body, true
}

// fileState is where a file of a batch is, as its rows show it.
type fileState int

const (
	fileWaiting      fileState = iota // queued
	filePaused                        // queued while the importer pauses
	fileUploading                     // its bytes are going up
	fileTranscribing                  // Zoom transcribes it
	fileSummarizing                   // saved, its recap being written
	fileReady                         // saved
	fileFailed                        // not transcribed: its Problem says why
	fileCanceled                      // stopped by the user
)

// busy reports a file the importer works on or will.
func (s fileState) busy() bool { return s <= fileSummarizing }

// fileProgress is how far a file has come: its state, and the share sent
// while it uploads.
type fileProgress struct {
	state fileState
	sent  float64
}

// progressOf is file f's progress: the importer's while it works on f,
// else the library's.
func progressOf(im *session.Importer, f store.BatchFile) fileProgress {
	if p, ok := im.Progress(f.ID); ok {
		switch {
		case f.State == store.FileSaved || p.Stage == zoomai.StageSummarizing:
			return fileProgress{state: fileSummarizing}
		case p.Stage == zoomai.StageTranscribing:
			return fileProgress{state: fileTranscribing}
		}
		return fileProgress{state: fileUploading, sent: p.Sent}
	}
	switch f.State {
	case store.FileSaved:
		return fileProgress{state: fileReady}
	case store.FileFailed:
		return fileProgress{state: fileFailed}
	case store.FileCanceled:
		return fileProgress{state: fileCanceled}
	}
	if im.Paused() != session.Running {
		return fileProgress{state: filePaused}
	}
	return fileProgress{state: fileWaiting}
}

// batchProgress is how far a batch has come.
type batchProgress struct {
	files            []fileProgress // in the batch's order
	total, ready     int
	failed, canceled int
	busy             bool
	pause            session.Pause
	done             float64 // the share done, from 0 to 1
	finished         float64 // the share of files finished, for the ring
	unknownLength    bool    // a file's metadata gave no duration
	length           time.Duration
}

// progressOfBatch is how far batch b has come.
func progressOfBatch(im *session.Importer, b store.BatchSummary) batchProgress {
	bp := batchProgress{total: len(b.Files), pause: im.Paused()}
	var sum float64
	finished := 0
	for _, f := range b.Files {
		fp := progressOf(im, f)
		bp.files = append(bp.files, fp)
		switch fp.state {
		case fileReady:
			bp.ready++
		case fileFailed:
			bp.failed++
		case fileCanceled:
			bp.canceled++
		}
		if !fp.state.busy() {
			finished++
			sum++
		} else {
			bp.busy = true
			switch fp.state {
			case fileUploading:
				sum += fp.sent / 3
			case fileTranscribing:
				sum += 1.0 / 3
			case fileSummarizing:
				sum += 2.0 / 3
			}
		}
		if d := fileLength(f); d > 0 {
			bp.length += d
		} else {
			bp.unknownLength = true
		}
	}
	if bp.total > 0 {
		bp.done, bp.finished = sum/float64(bp.total), float64(finished)/float64(bp.total)
	}
	return bp
}

// fileLength is how long file f's audio lasts: as its metadata says,
// else as its transcript does once saved; 0 when unknown.
func fileLength(f store.BatchFile) time.Duration {
	if f.Duration == 0 && f.Session.ID != "" && !f.Session.EndedAt.IsZero() {
		return f.Session.EndedAt.Sub(f.Session.StartedAt)
	}
	return f.Duration
}

// totalText is a length of many files, to the minute past one: "3 hr 1
// min", "45 sec".
func totalText(d time.Duration) string {
	if d < time.Minute {
		return lengthText(d)
	}
	return lengthText(d.Round(time.Minute))
}

// fileTitle is file f's name in Capy: its session's title once saved,
// else the name given it, else its file's.
func fileTitle(f store.BatchFile) string {
	switch {
	case f.Session.ID != "" && f.Session.Title != "":
		return f.Session.Title
	case f.Session.ID != "":
		return untitled(f.Session)
	case f.Title != "":
		return f.Title
	}
	return session.FileTitle(f.Name)
}

// minutesText is a length in whole minutes, "34 min", or seconds under one.
func minutesText(d time.Duration) string {
	if d < time.Minute {
		return strconv.Itoa(max(int(d.Seconds()), 1)) + " sec"
	}
	return strconv.Itoa(int(d.Round(time.Minute).Minutes())) + " min"
}

// problemShort says in a few words why a file failed, as its row does.
func problemShort(code string) string {
	switch code {
	case session.ProblemType:
		return "Not an audio file"
	case session.ProblemTranscribe:
		return "Couldn’t transcribe"
	case session.ProblemMissing:
		return "File not found"
	case session.ProblemTooLarge:
		return "Over 100 MB"
	case session.ProblemKey:
		return "API key refused"
	case session.ProblemTimeout:
		return "Took too long"
	case session.ProblemBusy:
		return "Zoom was busy"
	}
	return "Couldn’t upload"
}

// problemText says why a file failed, in a sentence.
func problemText(code string) string {
	switch code {
	case session.ProblemType:
		return "Zoom AI Services doesn’t take this type of file."
	case session.ProblemTranscribe:
		return "Zoom AI Services couldn’t transcribe this file."
	case session.ProblemMissing:
		return "The file is no longer where it was."
	case session.ProblemUnreadable:
		return "Capy couldn’t read the file."
	case session.ProblemTooLarge:
		return "The file is over 100 MB."
	case session.ProblemKey:
		return "Zoom refused the API key. Check it in Settings."
	case session.ProblemTimeout:
		return "Zoom AI Services took too long to answer."
	case session.ProblemBusy:
		return "Zoom AI Services was too busy. Try again shortly."
	}
	return "The file couldn’t be uploaded. Check your connection and try again."
}

// pauseText says why files wait.
func pauseText(p session.Pause) string {
	if p == session.PausedRecording {
		return "Paused while recording reconnects"
	}
	return "Paused · waiting for network"
}

// gist is the first line of recap that is no heading, without its
// Markdown: what a session's row says of it.
func gist(recap string) string {
	for line := range strings.Lines(recap) {
		line = strings.TrimSpace(strings.ReplaceAll(line, "**", ""))
		line = strings.TrimSpace(strings.TrimLeft(line, "-*•"))
		if line != "" && !strings.HasPrefix(line, "#") {
			return line
		}
	}
	return ""
}

// findFile is file id among batches, with its batch.
func findFile(batches []store.BatchSummary, id string) (store.BatchSummary, store.BatchFile, bool) {
	for _, b := range batches {
		for _, f := range b.Files {
			if f.ID == id {
				return b, f, true
			}
		}
	}
	return store.BatchSummary{}, store.BatchFile{}, false
}

// fileOfSession is the file saved as session id among batches, with its
// batch.
func fileOfSession(batches []store.BatchSummary, id string) (store.BatchSummary, store.BatchFile, bool) {
	for _, b := range batches {
		for _, f := range b.Files {
			if f.SessionID == id && id != "" {
				return b, f, true
			}
		}
	}
	return store.BatchSummary{}, store.BatchFile{}, false
}

// findBatch is batch id among batches.
func findBatch(batches []store.BatchSummary, id string) (store.BatchSummary, bool) {
	for _, b := range batches {
		if b.ID == id {
			return b, true
		}
	}
	return store.BatchSummary{}, false
}
