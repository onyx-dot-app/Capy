package views

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"
	"github.com/google/uuid"

	"github.com/onyx-dot-app/Capy/internal/audio"
	"github.com/onyx-dot-app/Capy/internal/lang"
	"github.com/onyx-dot-app/Capy/internal/session"
	"github.com/onyx-dot-app/Capy/internal/store"
	"github.com/onyx-dot-app/Capy/internal/views/kit"
	"github.com/onyx-dot-app/Capy/internal/zoomai"
)

// sheetFilesShown is how many files the sheet lists before it scrolls.
const sheetFilesShown = 8

// importSheet is files to import, read, as the sheet that confirms them
// shows them: the batch's name and languages, and the files turned away.
type importSheet struct {
	files    []store.BatchFile
	skipped  []skippedFile
	name     string
	lang     string
	target   string // "" for none
	problem  problem
	starting bool
	open     bool
}

// skippedFile is a file turned away, and why.
type skippedFile struct{ name, reason string }

// chooseFiles asks for files to import in the system's Open panel, which
// shows audio and video files only.
func (w *mainWindow) chooseFiles() {
	var extensions []string
	for _, ext := range zoomai.SpeechFileTypes() {
		extensions = append(extensions, strings.TrimPrefix(ext, "."))
	}
	go func() {
		paths, err := mygo.Dialog.Open(mygo.OpenDialogOptions{
			Parent:      w.win,
			Title:       "Import Audio Files",
			ButtonLabel: "Import",
			Message:     "Choose audio or video files to transcribe and summarize, up to 100 MB each.",
			Filters:     []mygo.FileFilter{{Name: "Audio and Video", Extensions: extensions}},
			Multiple:    true,
		})
		if err != nil {
			w.app.log.Warn("open panel", "err", err)
			return
		}
		w.addFiles(paths)
	}()
}

// addFiles reads the files at paths off the main thread, and shows those
// Zoom takes in the sheet, with the others and why they were turned away;
// to the sheet open, they are added. With none Zoom takes, a toast says why.
func (w *mainWindow) addFiles(paths []string) {
	if len(paths) == 0 {
		return
	}
	go func() {
		added, skipped := readFiles(paths)
		w.post(func() {
			s := w.sheet
			if s == nil {
				if len(added) == 0 {
					w.toasts = append(w.toasts, ui.Toast{Title: skippedText(skipped, "")})
					return
				}
				s = &importSheet{lang: w.app.prefs.ImportLanguage, target: w.app.prefs.ImportTarget}
				w.sheet = s
			}
			if s.starting {
				return
			}
			for _, f := range added {
				if !hasPath(s.files, f.Path) {
					s.files = append(s.files, f)
				}
			}
			s.skipped = skipped
			if strings.TrimSpace(s.name) == "" || s.name == s.defaultName(len(s.files)-len(added)) {
				s.name = s.defaultName(len(s.files))
			}
		})
	}()
}

// defaultName is the name of the sheet's first n files, as the batch is
// named unless the user names it.
func (s *importSheet) defaultName(n int) string {
	if n <= 0 {
		return ""
	}
	names := make([]string, n)
	for i, f := range s.files[:n] {
		names[i] = f.Name
	}
	return session.BatchName(names, time.Now())
}

func hasPath(files []store.BatchFile, path string) bool {
	for _, f := range files {
		if f.Path == path {
			return true
		}
	}
	return false
}

// readFiles reads the files at paths: those Speech Summarization takes,
// with their length and recording date, and the others, with why not.
func readFiles(paths []string) (added []store.BatchFile, skipped []skippedFile) {
	for _, path := range paths {
		name := filepath.Base(path)
		info, err := os.Stat(path)
		switch {
		case err != nil:
			skipped = append(skipped, skippedFile{name, "can’t be read"})
		case info.IsDir():
			skipped = append(skipped, skippedFile{name, "is a folder"})
		case store.ValidateFileName(name) != nil:
			skipped = append(skipped, skippedFile{strings.ToValidUTF8(strings.Map(printable, name), "?"), "has a name Capy can’t keep"})
		case !zoomai.IsSpeechFile(name):
			skipped = append(skipped, skippedFile{name, "isn’t audio"})
		case info.Size() > zoomai.MaxSpeechFileBytes:
			skipped = append(skipped, skippedFile{name, "is over 100 MB"})
		case info.Size() == 0:
			skipped = append(skipped, skippedFile{name, "is empty"})
		default:
			abs, err := filepath.Abs(path)
			if err != nil {
				skipped = append(skipped, skippedFile{name, "can’t be read"})
				continue
			}
			meta, _ := audio.Probe(abs) // a file it cannot read has no length to show
			added = append(added, store.BatchFile{ID: uuid.NewString(), Path: abs, Name: name, Size: info.Size(),
				Duration: meta.Duration, RecordedAt: meta.RecordedAt})
		}
	}
	return added, skipped
}

// printable is r, or a space for a character a name can't show.
func printable(r rune) rune {
	if unicode.IsPrint(r) {
		return r
	}
	return ' '
}

// skippedText says which files were turned away, and why: "notes.pdf
// isn’t audio and was left out." after is how the sentence ends, "" for a
// toast's.
func skippedText(skipped []skippedFile, after string) string {
	var sentences []string
	for i, s := range skipped {
		if i == 2 && len(skipped) > 3 {
			sentences = append(sentences, strconv.Itoa(len(skipped)-2)+" other files"+after+".")
			break
		}
		sentences = append(sentences, s.name+" "+s.reason+after+".")
	}
	return strings.Join(sentences, " ")
}

// dropOverlay shows that files dragged over the window are taken: they
// become a batch, confirmed in the sheet.
func dropOverlay(c *ui.Context) {
	p := kit.Colors(c)
	veil := ui.RGBA(255, 255, 255, 0.78)
	if c.Theme().Dark {
		veil = ui.RGBA(30, 29, 28, 0.78)
	}
	ui.Column(c).Absolute().Left(0).Top(0).Right(0).Bottom(0).Background(veil).Center().Children(func() {
		ui.Box(c).Absolute().Left(10).Top(10).Right(10).Bottom(10).Radius(10).Border(2, p.Accent).BorderStyle(ui.BorderDashed).
			Background(p.Accent.Alpha(0.05))
		ui.Column(c).AlignItems(ui.Center).Children(func() {
			ui.Icon(c, kit.ImportIcon).Size(40, 40).TextColor(p.Accent)
			ui.Text(c, "Drop to transcribe audio files").FontSize(17).FontWeight(600).Margin(14, 0, 0, 0)
			ui.Text(c, "Each file becomes a session with its own transcript and recap.").TextColor(p.Secondary).Margin(6, 0, 0, 0)
		})
	})
}

// importSheetView is the sheet that confirms files to import: the batch's
// name, its files, the languages of every file, Cancel and Transcribe.
func (w *mainWindow) importSheetView(c *ui.Context) {
	s := w.sheet
	if s == nil {
		return
	}
	s.open = true
	p := kit.Colors(c)
	ui.DialogBase(c, &s.open, func(backdrop, panel ui.Element) {
		backdrop.Background(ui.RGBA(0, 0, 0, 0.08)).Justify(ui.Start).Padding(64, 0, 0, 0)
		panel.Width(500).Radius(12).Background(p.Sheet).Border(0.5, ui.RGBA(0, 0, 0, 0.28)).
			Shadow(0, 22, 60, 0, ui.RGBA(0, 0, 0, 0.25)).Padding(20, 20, 16).AlignItems(ui.Stretch)
		ui.Column(c).Margin(0, 2, 14, 2).Children(func() {
			ui.Text(c, "Transcribe "+audioFilesText(len(s.files))).FontSize(15).FontWeight(600)
			ui.Text(c, filesTotalText(s.files)+" in total").FontSize(12).TextColor(p.Secondary).FontFeatures(kit.Tabular).Margin(2, 0, 0, 0)
		})
		sheetGroup(c, func() {
			kit.SettingRow(c, "Name", func() {
				ui.TextInput(c, &s.name).Label("Name").Width(260).Height(24).Disabled(s.starting)
			})
		})
		ui.Box(c).Height(12)
		sheetFiles(c, s)
		if len(s.skipped) > 0 {
			sheetNote(c, skippedText(s.skipped, " and was left out"))
		} else {
			ui.Box(c).Height(12)
		}
		sheetGroup(c, func() {
			kit.SettingRow(c, "Spoken language", func() {
				if code := s.lang; langSelect(c, &code, lang.Detectable(), "Language spoken in the files") {
					s.lang = code
				}
			})
		}, func() {
			kit.SettingRow(c, "Translate to", func() {
				names := []string{"None"}
				current := "None"
				for _, l := range lang.Targets() {
					names = append(names, l.Name)
					if l.Code == s.target {
						current = l.Name
					}
				}
				if chosen, ok := popUp(c, current, names, "Language to translate the files to"); ok {
					s.target = ""
					for _, l := range lang.Targets() {
						if l.Name == chosen {
							s.target = l.Code
						}
					}
				}
			})
		})
		sheetNote(c, "Applies to every file. Capy sends the audio to Zoom AI Services and keeps only the transcripts and recaps; your files stay where they are.")
		if s.problem.message != "" {
			ui.Row(c).Gap(8).AlignItems(ui.Center).Margin(0, 2, 12, 2).Children(func() {
				ui.Icon(c, kit.AlertIcon).Size(14, 14).TextColor(p.Danger)
				ui.Text(c, s.problem.message).FontSize(12).Grow(1).Basis(0)
				if s.problem.setKey && ui.Button(c, "Settings…").Height(24).Padding(0, 12).Clicked() {
					w.sheet = nil
					w.app.OpenSettings()
				}
			})
		}
		when := "when all " + strconv.Itoa(len(s.files)) + " are ready."
		if len(s.files) == 1 {
			when = "when it’s ready."
		}
		ui.Row(c).Gap(6).AlignItems(ui.Center).Margin(2, 2, 0, 2).Children(func() {
			ui.Icon(c, kit.BellIcon).Size(14, 14).TextColor(p.Secondary)
			ui.Text(c, "This takes a while. You’ll get a notification "+when).FontSize(12)
		})
		ui.Row(c).Gap(8).Justify(ui.End).Margin(16, 0, 0, 0).Children(func() {
			if ui.Button(c, "Cancel").Height(28).Padding(0, 14).Disabled(s.starting).Clicked() {
				s.open = false
			}
			label := "Transcribe " + filesText(len(s.files))
			if s.starting {
				label = "Starting…"
			}
			if ui.PrimaryButton(c, label).Height(28).Padding(0, 14).Disabled(s.starting || len(s.files) == 0).Clicked() {
				w.startImport(s)
			}
		})
	})
	if !s.open && !s.starting {
		w.sheet = nil
	}
}

// sheetGroup is a group of the sheet's rows, white on its gray.
func sheetGroup(c *ui.Context, rows ...func()) { kit.Group(c, rows...) }

// sheetNote explains the group above it, as a footnote does.
func sheetNote(c *ui.Context, text string) {
	ui.Text(c, text).FontSize(12).LineHeight(16.0/12).TextColor(kit.Colors(c).Secondary).Margin(6, 12, 16)
}

// sheetFiles lists the sheet's files: each one's name and length, its
// size when its length is unknown.
func sheetFiles(c *ui.Context, s *importSheet) {
	p := kit.Colors(c)
	list := ui.Scroll(c).Key("sheet-files").MaxHeight(30*sheetFilesShown).Radius(10).Background(p.Group).Border(1, p.Separator)
	list.Children(func() {
		for i, f := range s.files {
			ui.Row(c).Key(f.Path).Height(30).Padding(0, 12).Gap(8).AlignItems(ui.Center).Children(func() {
				if i > 0 {
					ui.Box(c).Absolute().Left(34).Right(12).Top(0).Height(1).Background(p.Separator)
				}
				ui.Icon(c, kit.AudioFileIcon).Size(14, 14).TextColor(p.Secondary)
				ui.Text(c, f.Name).SingleLine().Grow(1).Basis(0)
				length := sizeText(f.Size)
				if f.Duration > 0 {
					length = clock(f.Duration)
				}
				ui.Text(c, length).FontSize(12).TextColor(p.Secondary).FontFeatures(kit.Tabular)
			})
		}
	})
}

// startImport starts importing the sheet's files once the API key is read,
// remembers its languages for the next import, and shows the batch.
func (w *mainWindow) startImport(s *importSheet) {
	if s.starting {
		return
	}
	s.starting, s.problem = true, problem{}
	name := strings.TrimSpace(s.name)
	if name == "" {
		name = s.defaultName(len(s.files))
	}
	b := store.Batch{ID: uuid.NewString(), Name: name, Lang: s.lang, Target: s.target, CreatedAt: time.Now().UTC()}
	files := append([]store.BatchFile(nil), s.files...)
	a := w.app
	a.prefs.ImportLanguage, a.prefs.ImportTarget = s.lang, s.target
	a.savePrefs()
	go func() {
		_, err := a.client() // reads the Keychain: off the main thread
		if err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err = a.importer.Start(ctx, b, files)
			cancel()
		}
		w.post(func() {
			s.starting = false
			if err != nil {
				s.problem = explain(err)
				if !s.problem.setKey && !errors.Is(err, store.ErrInvalid) {
					a.log.Error("start import", "err", err)
				}
				if errors.Is(err, store.ErrInvalid) {
					s.problem = problem{message: "Capy can’t keep this name. Choose another."}
				}
				return
			}
			w.sheet = nil
			w.openBatch(b.ID)
			a.libraryChanged()
		})
	}()
}

// audioFilesText is "1 audio file" or "6 audio files".
func audioFilesText(n int) string {
	if n == 1 {
		return "1 audio file"
	}
	return strconv.Itoa(n) + " audio files"
}

// filesText is "1 File" or "3 Files".
func filesText(n int) string {
	if n == 1 {
		return "1 File"
	}
	return strconv.Itoa(n) + " Files"
}

// filesTotalText is how long files last together, "3 hr 1 min"; their
// size when a file's length is unknown.
func filesTotalText(files []store.BatchFile) string {
	var length time.Duration
	var size int64
	known := true
	for _, f := range files {
		length += f.Duration
		size += f.Size
		known = known && f.Duration > 0
	}
	if !known {
		return sizeText(size)
	}
	return totalText(length)
}

// sizeText is a file's size as Finder gives it: "820 KB", "12.4 MB".
func sizeText(n int64) string {
	switch {
	case n < 1000:
		return strconv.FormatInt(n, 10) + " bytes"
	case n < 1000*1000:
		return strconv.FormatInt((n+500)/1000, 10) + " KB"
	}
	return fmt.Sprintf("%.1f MB", float64(n)/1e6)
}
