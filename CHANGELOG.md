# Changelog

All notable changes to Habla. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions
follow [Semantic Versioning](https://semver.org/) (before 1.0, a minor version may break the configuration).

## [Unreleased]

### Added

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

[Unreleased]: https://github.com/ackstorm/habla/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/ackstorm/habla/releases/tag/v0.1.0
