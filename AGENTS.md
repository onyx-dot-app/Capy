# Capy — agent notes

macOS menu bar app (Go, mygo native UI) built on Zoom Scribe Live,
Translator, Summarizer and Speech Summarization. Read [README.md](README.md) first.

## Commands (from the repository root)

```sh
make test      # go test -race ./...
make lint      # gofmt, go vet (darwin, linux, e2e tag)
make e2e       # binary replay against the fake Zoom API, golden reports
make bundle    # go tool mygo build → build/darwin-arm64/Capy.app
```

## Rules

- **No cgo.** mygo's CLI builds with `CGO_ENABLED=0`. Native APIs go through
  `github.com/ebitengine/purego/objc`, only in `internal/audio/*_darwin.go`.
  Register Objective-C classes once: purego callbacks are never freed.
- `internal/session` joins recording, `internal/agent` joins Agent, and
  `main.go` joins the two: nothing else imports `internal/agent`, which
  imports of Capy only `store`, `features`, `profile`, `views/addon`,
  `views/kit` and `lang` (`internal/agent/boundary_test.go` checks it). `internal/zoomai`
  knows nothing of storage or UI; `internal/store` holds no SQL outside
  `sqlite/`.
- Every storage adapter must pass `storetest.Run`.
- The views know an add-on only through `internal/views/addon`, and share
  their look with it through `internal/views/kit`. Erase a session through
  `session.Purger`, never `Store.PurgeSession`, so its purge hooks run.
- Never log transcript text, translations, recaps or the API key; nor an
  agent's prompts, answers, tool arguments or results, or its CLI's stderr.
- Tests never run a real agent: give `cli.Env` a `Root` of the test's own.
  For a CLI, link `gateway/fake`'s fakeagent (or a test binary acting as it)
  as `claude` or `codex`; its streams are pinned by the recordings in each
  adapter's `testdata/`.
- View state changes from goroutines go through `Window.Update`; tests drive
  views with `ui.NewTester` (set `CAPY_SNAPSHOT_DIR` to save PNGs).

## Map

```
main.go, replay*.go    app entry, headless -replay (and -ask, -highlights) with its JSON report
highlights_eval.go     headless -highlights-eval: Highlights scored on testdata/eval
upload.go              headless -upload of audio files as a batch, with its JSON report
internal/audio         Source: Mic, System (purego), Mix, WAV replay; Probe (a file's length)
internal/zoomai        Scribe Live WS, Translate, Summarize, Speech Summarization, Decisions client
internal/lang          language catalog
internal/session       engine, highlighter (and FindHighlights of a saved session), recap, Transcribe (a file), Importer (batches), Markdown export
internal/store         Store interface, storetest contract, sqlite adapter
internal/keychain      API key in the Keychain ($ZOOM_API_KEY overrides)
internal/profile       who the user is, profile.json: Highlights and Ask read it
internal/views         tray, Capy's window, caption bar, settings, Recently Deleted, Import
internal/views/kit     Capy's palette, icons and widgets, shared with an add-on's UI
internal/views/addon   the slots the views offer one add-on, and the Host it may call
internal/features      the feature gate, features.json
internal/agent         Agent, the add-on: Module, Capy mcp, register.go (the agents)
  prefs/               agent.json
  threads/, sqlite/    the thread store contract, its storetest, agent.db
  tools/               Capy's MCP server and its tools
  ask/                 Ask: threads, prompts, runs, edits, Continue in…, its export
  polish/              Highlight polish: passes with no tools, the prompt, the ops' guards
  gateway/             the Agent contract and the Gateway with its runs; cli/ finds
                       and runs CLIs; claudecode/, codex/ the built-in agents;
                       fake/ a scripted agent for tests, cmd/fakeagent the CLI stand-in
  ui/                  Settings › Agents; Ask's inspector and caption bar panel (one
                       panel on two surfaces: thread, answer), its export
internal/fakezoom      scripted fake of the four APIs for tests
e2e/                   replay E2E (build tag e2e) and fixtures
scripts/               icon and fixture generators
```
