# Habla

Push-to-dictate for the Linux desktop through OpenAI-compatible, Gemini or ElevenLabs (TTS) speech APIs.

## Commands

- Always `GOTOOLCHAIN=local` (never auto-download Go 1.26; `golang.org/x/sys` is pinned at v0.47.0 for Go 1.25).
- Tests: `GOTOOLCHAIN=local timeout 300 go test ./...`; vet: `GOTOOLCHAIN=local go vet ./...`.
- Live gateway: `HABLA_LIVE=1 GOTOOLCHAIN=local timeout 300 go test ./internal/gateway/ -run Live -v`.
- Static binary: `CGO_ENABLED=0 GOTOOLCHAIN=local go build -trimpath -ldflags='-s -w' -o dist/habla .`
- Release: move `[Unreleased]` in `CHANGELOG.md` to `## [X.Y.Z] - date` (and its compare link), commit, tag `vX.Y.Z`
  (annotated), push. The Release workflow's "Notes" step puts that section into `.github/release-notes.md`'s
  `@CHANGES@` and fails when it is missing or empty.
- Logs: `tail -f ~/.local/state/habla/habla.log` (tests: `$XDG_STATE_HOME` is the test's dir). `log_level` (default
  warn) or `--log error|warn|info|debug` on any subcommand; info = one line per pipeline step with ms (one `http` line per
  request), debug adds the streamed chunks, trim and hold transitions (never raw keys). Never log the key, headers or dictated/spoken text (`chars=`).
  Every line carries `[delta_ms=N]` after the level: ms since the previous line of that process (0 for the first).

## Layout

- `plugins/habla`: shared Claude Code/Codex plugin: the `habla` skill configures user TOML and speaks through `habla say`;
  its bundled schema example is checked against the real parser by `internal/release/plugin_test.go`.
- `main.go`: `habla listen` → `listen.Main`, `habla say` → `say.Main`, `habla stats` → `stats.Main`, `habla live` → `live.Main`, `habla tray` → `tray.Main`.
- `internal/say`: `habla say`. `config.TTS.Delivery` selects the style voice/instructions and warns if a style is inactive. One `gateway.Speak` request → `--out FILE`, or
  `say.wav` played by `pw-play`/`paplay` while holding `say.pid` (no `state`); SIGTERM (`--stop`) terminates the player, exit 0.
- `internal/hotkey`: pure hold-to-talk logic: `Parse`/`Label`/`PortalTrigger`, `Keysym` table, `GrabMasks`, `DropAutoRepeat`, `PortalEvent`, `Hold` (`Event` slices → start/stop/cancel; `Active()`).
- `internal/tray` (`logic.go`): pure tray helpers (`AcquireLock`, `Install{Autostart,DesktopEntry}`, `Levels`, `PillHint`, `PlacePill`, `MenuChoices`, `EnqueueListen`, `PortalDesktop`, `HablaPath`, `AppID`), `Icons` (embedded SVGs). No GTK in `logic.go`; `config.SaveChoice` writes the menu choice into config.toml.
  `pill.go`: the recording pill, shown/hidden by `refresh` from `state` (so every way to record shows it): an X11
  POPUP placed by `PlacePill` on the active window's monitor, a layer-shell overlay, or (Wayland without layer-shell)
  the `notice` (`desktop.Critical` → `CloseNotification`).
- `internal/tray` (GTK side): `tray.go` `Main` (`--install-autostart` = autostart + portal entry + icons in
  `~/.local/share/icons`; else lock → `gtk.Load` → indicator, menu, `refresh` from `rundir.ReadState` + `config.Load`,
  `habla listen <flag>` one at a time, reaped by a goroutine → `gtk.Idle`), `hotkey_x11.go` (xgb grab on its own
  connection, Esc via `GrabKeyboard` while held, `xembedTray`), `portal.go` (`runPortal`: KDE/GNOME Wayland
  GlobalShortcuts on its own session-bus connection, `Register` → `CreateSession` → `BindShortcuts` → `Hold`; one
  goroutine for the process), `watch.go` (inotify on the runtime and config dirs).
  `init` locks the main OS thread for GTK. Icons are extracted to `$XDG_RUNTIME_DIR/habla/icons/hicolor/...`.
- `internal/config`: `Load()` = `[general]` / `[tray]` (hotkey, paste, paste_enter: tray only; `habla listen` uses
  `--paste`/`--paste_enter` → `Config.Paste`/`PasteEnter`) / `[split]` defaults ← config.toml decoded as `file` (per-service `providers`, `stt.transforms`, top-level `styles`), each service's
  active provider resolved into `STT`/`LLM`/`TTS`/`Live` (`Use` for `--provider`, `UseStyle` for `say`/`live --style`, `""` = none); `SaveChoice` writes
  into the active provider (one key line in place, re-parsed before the atomic rename) and `setKey`/`lineOf` skip multi-line strings;
  `Conn`, `TransformModel`, and strict unknown-key errors. An invalid file returns nil + the error: listen, say and tray
  startup exit 1 through `desktop.ConfigError`; the running tray's `refresh` keeps its last valid config.
- `internal/logs`: `Setup(command, --log)` once per `Main` (slog default: `habla.log`, 0600, opened per record (O_APPEND, one write), rotated to `.1` over 1 MB at `Setup`, which also chmods the dir 0700; unwritable = skipped; plus info/debug to stderr on a terminal: warn/error print their own message), `SetLevel(log_level)` after `config.Load`
  (ignored when `--log` was given). `--log` reaches child habla processes through the internal `HABLA_LOG` env var.
- `internal/stats`: stats.jsonl (one record per dictation / say, counts only), `Calls` prices gateway replies
  (x-litellm-response-cost header → `[prices]` → none), `Main` = `habla stats` (`Summarize`, `History`).
- `internal/rundir`: `$XDG_RUNTIME_DIR/habla` (`Claim`, `Release`, `RunningPID`, `Alive`, `ReadState`, `WriteState`,
  `Delivery`). Delivery phases carry their owner PID in `state` and use Claim's directory lock, so completion after
  releasing `listen.pid` cannot overwrite a newer recording. Copy failures persist until the next recording.
- `internal/audio`: recorder process, `Header`/`HeaderAt`/`Data` (WAV), `Voiced`/`Trim` (silence; all-silent trims to nothing). `listen.minRecording` (300 ms): shorter or all-silent recordings send no request; tests' `recording(t)` waits for that much audio.
- `internal/gateway`: `Credential`, `New(conn, key)`, shared transports per scheme+host, `Transcribe(ctx, model, language, pcm, -1)` streams, named `Transform`, `Speak`; `Speak` with `api = "elevenlabs"` POSTs `/text-to-speech/{voice ID}?output_format=pcm_24000` and wraps the PCM in `audio.HeaderAt(24000, n)` (reply header `character-cost` → `Call.In`; `voices` entries `Name=ID` are mapped to the ID by `config.TTS.Delivery`/`LiveDelivery`); `gemini.go` generates requests, streams transcription as base64 WAV, and uses native `audio/wav` without `Unwrap`.
  (`audio.Unwrap` on the OpenAI-compatible speech bridge: LiteLLM wraps Gemini's whole WAV in a second header, BerriAI/litellm#44541, so its C2PA chunk
  plays as ~126 ms of noise).
- `internal/desktop`: `Copy`, `Paste(keys, enter)` (XTEST on X11, wtype/ydotool on Wayland; Enter 100 ms later with the same tool), `Notify` (D-Bus; a var for tests), `ConfigError`, `Terminal`, `Critical`/`CloseNotification`, `ActiveWindow` (X11).
- `internal/listen`: `Main`, `runSession` (a loop over parts: SIGUSR2 while busy, from `--start`/`--toggle` via
  `startOrContinue`, records another part), `dictation` (`record` → `part-<pid>-<n>.wav`; `finish`: wait for every
  transcript → join → one transform → `deliver`; SIGTERM while busy discards it all), `upload` (STT and LLM credential prefetch + streaming
  (polls the growing file every `general.upload_interval_ms`, default 50; the recorder's stop ends the wait at once) + file fallback; `finish(wav)` takes the renamed path; `[split]`: a `splitter` cuts the stream at long pauses into
  pieces, one request each, `joined` in order; any failed piece → one file upload).
- `internal/testenv`: test-only. `Setup` puts links to the test binary on PATH; `WriteConfig` merges overrides into the generated file; `HelloWAV`/`HelloPCM` generate the test clip (2.0 s, 16 kHz mono: a 200 Hz tone whose per-frame level mimics a recorded "Hello world.": voiced frames 0 and 12-50 and 66-72, trims to [0, 53120) at -45 dB, voice ends at 1.46 s), `Speech16k` converts a TTS WAV for live tests; `TestMain` calls `testenv.Fake()`.
  `fake-portal` (`portal.go`) is a godbus GlobalShortcuts portal for `dbus-run-session` (`$HABLA_FAKE_PORTAL` mode),
  and the notification daemon (`notifications` lines).

## Gotchas

- Gemini `gemini-3.5-transcribe` returns its text in `audioTranscription.text`; general models use `text`.
- SIGUSR2's default action kills: every `habla listen` process that can be `busy` Notifies it (background session) or
  Ignores it (interactive, `--retry`). A session releases `listen.pid` before copying and never touches `current.wav`
  or part files after that (a new session may own them). Tests that hold the fake gateway (`Replies.HoldSTT/HoldChat`)
  register `release` after `setup`: the server's `Close` waits for held requests, and cleanups run last-in first-out.
- The tray grabs Esc, Return and KP_Enter on X11 only while the state is `busy` or `transform` (`grabBusyKeys`); `Hold.Feed` maps an
  unheld Esc to cancel and an unheld Enter to send (`habla listen --send` → `rundir.SendMark`, read and removed by `deliver`:
  paste + Enter; a new session removes a stale one). `Event.State` tells a plain Enter from a `<Super>Return` hotkey.
  After a send the tray's `lockFocus` grabs the keyboard and pointer until `grabBusyKeys(false)` (or 60 s); XTEST keys
  go to a grabbing client, so `pasteX11` first waits (`waitUngrabbed`, 1 s) until no client holds the keyboard.

- Tray hotkey auto-repeat: xgb hands events over one at a time, so a repeat's release+press pair is not always queued
  together; after a release the loop waits `autoRepeatGap` (20 ms) for the press. Xvfb auto-repeats XTEST keys, and
  `TestTrayHoldToTalkGUI` holds the key 1.2 s: with no gap it fails ("the recording ended while the key was held").
- Tray GUI tests (`HABLA_GUI=1 ... go test ./internal/tray/ -v`): `run` (called by `TestMain`) builds the real binary
  with `CGO_ENABLED=0`; the tray runs under `dbus-run-session` in its own process group. The tray has no ready signal:
  the tests wait for the 5 s *No tray* notice (Xvfb has no tray host), which comes after the hotkey grab.
- The watch has no `IN_MODIFY`: the recorder writes `current.wav` continuously; every file the tray reads ends with a
  create, rename, delete or close-after-write.

- The test binary is not named `habla`, so `rundir.Alive` rejects it: in-process sessions are not "recording" to
  `ReadState`. Tests wait for `listen.pid` instead, and use `testenv.HablaProcess` (sleep via a `habla` link)
  as a live holder. `listen.Self` points `--start` at the `habla` link, which `TestMain` dispatches to `Main`.
- `cmd.Start()` returning does not guarantee `/proc/<pid>/cmdline` is set: `testenv.HablaProcess` waits (bounded)
  for a non-empty cmdline before `rundir.Alive` can see the child.
- The fake recorder writes a size-0 header while recording; Habla never forwards a recorder header (the gateway
  rejects size 0); it sends `audio.Header(-1)` and the data chunk's PCM.
- `--session` marks fd 3 (the ready pipe to the `--start` parent) close-on-exec at once (`syscall.CloseOnExec(3)`):
  otherwise the recorder inherits it and `--start` blocks until the recording ends.
- The session removes a stale `current.wav` before starting the recorder: a file left by a killed session would
  otherwise be streamed (and spliced with the new audio).
- X11 paste uses `xtest.FakeInputChecked(...).Check()`: the unchecked variant drops server errors, so a rejected
  keystroke would read as "Pasted".
- `go.mod` says `go 1.25.0` (x/sys v0.47.0 declares it); `go 1.25` makes the build fail with "updates to go.mod needed".
- Measured live runs 2026-10-16 (release → copied, none / clean; measured with the original recorded clip that `testenv.HelloWAV` replaced; later live runs use a synthesized "Hello world." from the live TTS): 0.79 s / 1.04 s and 0.98 s / 0.61 s;
  the gateway varies by about ±0.3 s, so treat these as a range, not a number.
- `xclip`/`wl-copy` fork to serve the selection: never give them a stdout/stderr pipe.
- Interactive mode: cbreak with `TCSETS` (TCSANOW) keeps typed-ahead keys; restore with `TCSETSW`.
- Never rely on the dev box's tools: tests put only fakes on PATH and a Wayland session with no display or bus.
  The X11 XTEST paste is checked by hand on the owner's MATE host.
- Hand checks on the owner's host (streaming has never run against a real recorder; the dev box has no pw-record or
  parecord): run `habla listen --start` / `--stop` once with pw-record and once with `PATH` lacking it (parecord),
  compare the transcripts, and dump the header while recording and after stop:
  `od -A d -t x1 -N 48 $XDG_RUNTIME_DIR/habla/current.wav`.
