# Changelog

All notable changes to Habla. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions
follow [Semantic Versioning](https://semver.org/) (before 1.0, a minor version may break the configuration).

## [Unreleased]

### Changed

- `habla live` has its own session limit, `max_seconds` in the live provider (default 3600, 60 minutes), instead of
  `general.max_seconds` (300), which now limits only dictation.

## [0.1.11] - 2026-10-26

### Changed

- Live file tools are on by default: a live provider without `tools` gets them (a model that takes no tools ignores
  them, as a Gemini bridge without tool support does); `tools = false` turns them off.

## [0.1.10] - 2026-10-26

### Added

- `habla live` can create files where it runs, and edit or delete the ones it created in that conversation, so you
  can say "write a summary to notes/today.md". It needs `tools = true` in the live provider (the default `openai`
  provider has it; add it to a gateway provider whose model takes tools); `--no-tools` turns it off for a run. Each
  call is a `Tool:` line in the transcript.

## [0.1.9] - 2026-10-26

### Added

- The tray also plays a short "pip" when Enter sends a dictation (paste, then Enter); `[tray] sound = false` mutes it
  with the others.
- KDE and GNOME on Wayland get send and discard: the portal also binds the hotkey's modifiers plus Enter and Esc
  (`Super+Enter`, `Super+Esc`); the desktop asks once to confirm them. The README shows the sway and Hyprland lines.
- Enter while the hotkey is still held stops and sends at once (X11 and the portal), and `habla listen --send` is
  accepted while recording.

## [0.1.8] - 2026-10-26

### Added

- The tray plays a rising two-note tone when the hotkey starts recording and a falling one when it stops, so a press
  that missed the hotkey (Space a moment before Super) is heard at once. `[tray] sound = false` turns it off.

### Changed

- The tray menu always shows *STT provider* and *LLM provider*, with the active one ticked, even when only one is
  configured.

## [0.1.7] - 2026-10-23

### Changed

- `habla tray` starts the tray in the background (its own session, no terminal) and returns once the icon is up; it
  still refuses to start a second tray. `habla tray --foreground` keeps it attached to the terminal, and the autostart
  entry uses it (`habla tray --install-autostart`, run by the installer, rewrites the entry). The installer restarts
  the tray this way on an upgrade.

## [0.1.6] - 2026-10-23

### Changed

- The tray menu offers only what dictation uses: *Transform*, then *STT provider* and *LLM provider* (with two or
  more providers). The language, model, voice, TTS and Live pickers are gone; `language` still applies from
  config.toml, and `languages` and `models` are accepted but unused.

## [0.1.5] - 2026-10-23

### Added

- `habla listen --transform NAME` picks a `[stt.transforms.*]` for one run; `--transform ""` turns the provider's
  transform off.

### Changed

- The installer restarts a running tray after an upgrade, warns about a missing API key only on a first install (no
  config yet), and no longer checks for the pre-0.1.0 `active` key.

## [0.1.4] - 2026-10-23

### Fixed

- The focus lock after Enter lasts through copying, until the paste: it used to end before the copy, leaving time
  to switch windows between the paste and Enter.

## [0.1.3] - 2026-10-22

### Changed

- After Enter while busy, Habla holds the keyboard and mouse until it pastes (at most 60 s; Esc still cancels), so
  Alt+Tab or a click can't move the paste to another window. The pill says *Will paste and send (keep this window
  focused)*.

## [0.1.2] - 2026-10-22

### Fixed

- The keypad's Enter sends a busy dictation too, not only the main Enter.

## [0.1.1] - 2026-10-20

### Added

- `habla say --style NAME` and `habla live --style NAME` pick a `[styles.*]` for one run; `--style ""` turns
  the provider's style off.
- Enter while transcribing or refining pastes the dictation and presses Enter once it is ready (X11, like Esc to
  cancel; `habla listen --send` elsewhere). The pill shows *ESC / ↵ cancel / send*, then a tick and *Will paste and
  send*.
- The recording pill times transcription and transform: *Transcribing (0.8s)*, *Refining text (1.4s)*; the recording
  time stays beside them.

### Changed

- A tap of the hotkey (under 300 ms) or a recording with nothing above `trim_below_db` is no longer sent to the
  speech-to-text API: Habla says *Nothing heard.* at once instead of paying for a request (`trim_below_db = 0` still
  sends everything over 300 ms).

## [0.1.0] - 2026-10-20

### Added

- `habla listen`: hold-to-talk dictation. Audio streams to the speech-to-text API while you record; the transcript is
  copied to the clipboard and, optionally, pasted into the focused window, then Enter (`--paste`, `--paste_enter`;
  X11 built in, `wtype` or `ydotool` on Wayland). Works in any language the provider supports.
- Optional LLM transform with named prompts (`clean`, `punctuation`, `english`, `commit`), long-dictation splitting at
  pauses, and continuing a dictation while it is still being transcribed.
- `habla tray`: tray icon with provider, model, language, transform and voice menus and the *Also paste
  automatically* / *And press Enter* checkboxes (`[tray]`) saved to `config.toml`, a recording pill with a live
  level meter, and a hold-to-talk hotkey on X11, KDE and GNOME (XDG GlobalShortcuts portal), with sway and Hyprland
  bindings.
- `habla say`: text to speech with OpenAI-compatible, Gemini or ElevenLabs providers, shared delivery styles and
  `--out FILE`.
- `habla live` (experimental): spoken realtime conversation about an optional text file, with a saved transcript.
- `habla stats`: cost, token and latency summaries per service and per day, week or month.
- Providers per service (`stt`, `llm`, `tts`, `live`) with OpenAI-compatible and Gemini APIs; `--provider NAME` for
  one run. The generated config ships OpenAI and Gemini providers and an inactive ElevenLabs one.
- Log file with per-step timings (`[delta_ms=N]`) and a configurable upload interval
  (`[general] upload_interval_ms`, 50-1000, default 50).
- `habla` skill for Claude Code and Codex: configuration and spoken replies via `habla say`.
- One static binary (amd64, arm64) installed by `install.sh` with checksum verification and tray autostart.

[Unreleased]: https://github.com/ackstorm/habla/compare/v0.1.11...HEAD
[0.1.11]: https://github.com/ackstorm/habla/compare/v0.1.10...v0.1.11
[0.1.10]: https://github.com/ackstorm/habla/compare/v0.1.9...v0.1.10
[0.1.9]: https://github.com/ackstorm/habla/compare/v0.1.8...v0.1.9
[0.1.8]: https://github.com/ackstorm/habla/compare/v0.1.7...v0.1.8
[0.1.7]: https://github.com/ackstorm/habla/compare/v0.1.6...v0.1.7
[0.1.6]: https://github.com/ackstorm/habla/compare/v0.1.5...v0.1.6
[0.1.5]: https://github.com/ackstorm/habla/compare/v0.1.4...v0.1.5
[0.1.4]: https://github.com/ackstorm/habla/compare/v0.1.3...v0.1.4
[0.1.3]: https://github.com/ackstorm/habla/compare/v0.1.2...v0.1.3
[0.1.2]: https://github.com/ackstorm/habla/compare/v0.1.1...v0.1.2
[0.1.1]: https://github.com/ackstorm/habla/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/ackstorm/habla/releases/tag/v0.1.0
