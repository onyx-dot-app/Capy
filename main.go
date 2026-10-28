// Capy is a macOS menu bar app that shows Zoom AI Services at work: live,
// translated captions of the microphone or of what the Mac plays, with
// Scribe, Translator and Summarizer.
//
//	go run .                   # the app
//	go run . -replay talk.wav  # a session from a file, headless
//	go run . -replay talk.wav -ask "What are the action items?" -agent claude-code
//	go run . -replay talk.wav -highlights -listener "Alex Rivera"
//	go run . -replay talk.wav -highlights -polish -agent claude-code
//	go run . -highlights-eval -report out/highlights-eval.json testdata/eval/*.json
//	go run . mcp               # Capy's tools for an agent, over stdio
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/egoist/mygo"

	"github.com/onyx-dot-app/Capy/internal/agent"
	"github.com/onyx-dot-app/Capy/internal/features"
	"github.com/onyx-dot-app/Capy/internal/session"
	"github.com/onyx-dot-app/Capy/internal/store"
	"github.com/onyx-dot-app/Capy/internal/store/sqlite"
	"github.com/onyx-dot-app/Capy/internal/views"
	"github.com/onyx-dot-app/Capy/internal/zoomai"
)

// baseURLEnv points -replay's Zoom client at another server: https, or a
// loopback http server such as the E2E tests' fake. The app ignores it, so
// an environment variable cannot redirect a user's key and transcripts.
const baseURLEnv = "CAPY_API_BASE_URL"

func main() {
	// Capy mcp is started by an agent: no app, no window, no lock.
	if len(os.Args) > 1 && os.Args[1] == "mcp" {
		os.Exit(runMCP(os.Args[2:]))
	}
	var replay replayOptions
	flag.StringVar(&replay.wav, "replay", "", "run one session from a 16 kHz mono PCM16 WAV file instead of the app")
	uploading := flag.Bool("upload", false, "import the audio or video files named after the flags, as one batch, instead of the app")
	evaluating := flag.Bool("highlights-eval", false, "replay the labeled meetings named after the flags through Zoom Decisions and score Highlights, instead of the app")
	var up uploadOptions
	flag.StringVar(&up.name, "name", "", "upload: the batch's name (default: the start the files' names share)")
	flag.StringVar(&up.target, "translate-to", "", "upload: translate the transcripts to this language")
	flag.StringVar(&replay.langA, "lang-a", "en-US", "replay: spoken language, or auto to detect it; upload: the language spoken")
	flag.StringVar(&replay.langB, "lang-b", "zh-CN", "replay: translation language")
	flag.BoolVar(&replay.translate, "translate", false, "replay: translate the turns to -lang-b")
	flag.StringVar(&replay.report, "report", "", "replay, upload, highlights-eval: write a JSON report to this path")
	flag.StringVar(&replay.db, "db", "", "replay, upload: SQLite file (default: a temporary one)")
	flag.BoolVar(&replay.realtime, "realtime", true, "replay: send audio at the pace it was recorded")
	flag.StringVar(&replay.ask.question, "ask", "", "replay: ask this about the session, with -agent, after its last turn")
	flag.IntVar(&replay.ask.at, "ask-at", 0, "replay: ask after this many turns, while it records")
	flag.StringVar(&replay.ask.agent, "agent", "", "replay: the agent -ask asks, claude-code or codex")
	flag.StringVar(&replay.ask.agentPath, "agent-path", "", "replay: the agent's program, if not where Capy finds it")
	flag.BoolVar(&replay.highlights.on, "highlights", false, "replay: mark the turns that matter with Zoom Decisions")
	flag.BoolVar(&replay.polish, "polish", false, "replay: polish the highlights with -agent, as Polish with agent does")
	flag.StringVar(&replay.highlights.listener, "listener", "", "replay: the listener's name, for highlights")
	flag.StringVar(&replay.highlights.also, "also-called", "", "replay: the listener's other names, comma-separated")
	flag.DurationVar(&replay.highlights.every, "highlight-every", session.DefaultHighlightEvery, "replay: how often highlights are checked")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if *uploading {
		up.files, up.language, up.report, up.dbPath = flag.Args(), replay.langA, replay.report, replay.db
		os.Exit(runUpload(up, log))
	}
	if *evaluating {
		os.Exit(runHighlightsEval(flag.Args(), replay.report, log))
	}
	if replay.wav != "" {
		os.Exit(runReplay(replay, log))
	}
	if err := runApp(log); err != nil {
		log.Error("capy", "err", err)
		os.Exit(1)
	}
}

// newClient makes the app's Zoom client, for api.zoom.us.
func newClient(key string) (*zoomai.Client, error) {
	return zoomai.NewClient(key)
}

// dataDir is Capy's data directory, ~/Library/Application Support/Capy,
// which the app and Capy mcp share.
func dataDir() (string, error) {
	mygo.App.SetName("Capy")
	dir, err := mygo.App.Path(mygo.PathUserData)
	if err != nil {
		return "", fmt.Errorf("data directory: %w", err)
	}
	// mygo creates it 0755; it holds private transcripts.
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", fmt.Errorf("data directory: %w", err)
	}
	return dir, nil
}

// gate is the feature gate: features.json in dir over the defaults.
func gate(dir string, log *slog.Logger) features.Gate {
	g, err := features.Local(filepath.Join(dir, "features.json"))
	if err != nil {
		log.Warn("features", "err", err)
	}
	return g
}

// version is Capy's, as the packaged app gives it.
func version() string {
	if v := mygo.App.Version(); v != "" {
		return v
	}
	return "dev"
}

// runMCP serves Capy's tools to the agent that started it, on stdin and
// stdout; logs go to stderr.
func runMCP(args []string) int {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	return agent.ServeMCP(context.Background(), args, agent.MCPDeps{
		DataDir: dataDir, Gate: func(dir string) features.Gate { return gate(dir, log) },
		Stderr: os.Stderr, Version: version(),
		OpenLibrary: func(ctx context.Context, path string) (store.Store, func() error, error) {
			db, err := sqlite.Open(ctx, path)
			if err != nil {
				return nil, nil, err
			}
			return db, db.Close, nil
		},
	})
}

func runApp(log *slog.Logger) error {
	dir, err := dataDir()
	if err != nil {
		return err
	}
	if !mygo.App.RequestSingleInstanceLock() {
		return nil // the running Capy takes over
	}
	ctx := context.Background()
	db, err := sqlite.Open(ctx, filepath.Join(dir, "capy.db"))
	if err != nil {
		return fmt.Errorf("open library: %w", err)
	}
	defer db.Close()
	if err := session.RecoverInterrupted(ctx, db); err != nil {
		log.Warn("recover interrupted sessions", "err", err)
	}
	self, err := mygo.App.Path(mygo.PathExe)
	if err != nil {
		return fmt.Errorf("executable: %w", err)
	}
	g := gate(dir, log)
	ag, err := agent.New(ctx, agent.Deps{Store: db, Gate: g, Dir: dir, Self: self, Log: log})
	if err != nil {
		return err
	}
	defer ag.Close()
	purger := session.NewPurger(db)
	purger.OnPurge(ag.PurgeHook()) // gate or not: Delete Now erases the threads

	app := views.NewApp(db, purger, filepath.Join(dir, "settings.json"), log, newClient, ag.AddOn(), g)
	mygo.App.OnSecondInstance(func([]string, string) { app.OpenWindow("") })
	mygo.App.OnNotificationClick(func(id string) { app.OpenWindow(id) })
	// A menu bar app keeps running with no window open.
	mygo.App.OnWindowAllClosed(func() {})
	mygo.App.WhenReady(func() {
		if err := app.Start(); err != nil {
			log.Error("menu bar item", "err", err)
			mygo.App.Quit()
		}
	})
	return mygo.App.Run()
}
