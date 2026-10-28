# Capy

**Live captions, translation and recaps of any conversation on your Mac.**

Capy is a macOS app that captions, translates and summarizes any conversation
in real time: a meeting, a video, a call in your headphones. It runs on
[Zoom AI Services](https://zoom.ai): get an API key there and try it in minutes.

Capy is an independent project, not affiliated with or endorsed by Zoom.

## Powered by Zoom AI Services

| Zoom AI Service | What Capy does with it |
|---|---|
| **Scribe** | Live captions over every app, from your microphone or anything your Mac plays, with speaker labels |
| **Translator** | Real-time translation of every caption, across 10 languages |
| **Summarizer** | A recap with action items for every session |
| **Speech Summarization** | Transcripts and recaps of audio and video files |
| **Decisions** | Highlights of what matters: requests for you, decisions, debates, action items |

Audio is never stored. Transcripts stay on your Mac.

## Requirements

- macOS 13+ (14.2+ to caption what the Mac plays)
- Go 1.27.1+
- A Zoom AI Services API key from [zoom.ai](https://zoom.ai)

## Quick start

```sh
make run        # run from source; a capybara appears in the menu bar
make run-fresh  # same, on an empty library, as at first launch
make bundle     # build/darwin-arm64/Capy.app and Capy-<version>.dmg
```

Then open **Settings** (gear in the sidebar, or **Settings…** in the menu) and
paste your API key.

### API key

1. Sign in at [zoom.ai](https://zoom.ai).
2. Create an API key with the permissions you need: **Scribe**, **Translator**,
   **Summarizer** (with **Speech summarization** for Import) and **Decisions**
   (for Highlights).
3. Copy the key; it is shown only once.

The key is stored in the macOS Keychain; `$ZOOM_API_KEY` overrides it. Other
programs you run can read it from the Keychain without a prompt, so limit the
key to the permissions above.

### Permissions

macOS asks for **Microphone** on first use, and for **System Audio Recording**
the first time Captions listens to "This Mac". Capy hears audio only, never the
screen. Notifications work only in the bundled app.

### Signing

`make bundle` signs ad hoc, so macOS asks for permissions again after every
rebuild. To keep them, save your signing identity (gitignored):

```sh
security find-identity -v -p codesigning                     # list yours
echo 'Apple Development: Jane Doe (TEAMID)' > .signing-identity
```

`entitlements.plist` grants the microphone entitlement the hardened runtime
requires. To distribute, set `macos.signingIdentity` (Developer ID) and
`macos.notarize` in `mygo.json`; see mygo's
[distribution guide](https://github.com/egoist/mygo/blob/main/docs/distribution.md).

## Try it without speaking

Replay a recording through Scribe and Translator, headless, with a JSON report:

```sh
read -r -s ZOOM_API_KEY && export ZOOM_API_KEY
go run . -replay e2e/testdata/captions-en.wav -lang-a en-US -lang-b zh-CN -translate -report /tmp/capy.json
```

## Development

Capy is one Go binary built with [mygo](https://github.com/egoist/mygo)'s
native UI: no cgo, no webview, no server.

```sh
make test      # unit + integration (fake Zoom API, real SQLite)
make lint      # gofmt, go vet
make e2e       # replay fixtures against the fake API; reports must match e2e/testdata/*.golden.json
make e2e-live  # same fixtures against the real API (needs ZOOM_API_KEY)
```

Contributors: read [AGENTS.md](AGENTS.md) for the rules and package map.


## License

[MIT](LICENSE)
