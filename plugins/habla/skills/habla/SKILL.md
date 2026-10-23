---
name: habla
description: Configure Habla (config.toml, providers, models, voices, transforms, styles, troubleshooting) and speak to the user out loud with `habla say`, including opt-in spoken notifications ("tell me out loud when you finish", "set up spoken notifications"). Not for Claude or Codex settings unrelated to speech, nor Habla development.
---

# Habla

Two parts: configure Habla, and speak to the user through `habla say`.

## Configure

Help the user configure Habla by editing its TOML file. The tray is for quick choices, not a full settings editor.

Read [references/configuration.md](references/configuration.md) before editing. This reference travels with the
plugin; it does not require a Habla checkout. If a checkout is available, check `internal/config/config.go` for
version differences and use that version's schema.
For a complete mixed-provider config (Gemini STT, OpenAI transforms, ElevenLabs TTS), see
https://github.com/ackstorm/habla/blob/main/examples/config.toml (`examples/config.toml` in a checkout).

### Find and inspect

Use the file the user identifies; otherwise use `$XDG_CONFIG_HOME/habla/config.toml`, falling back to
`~/.config/habla/config.toml`. Do not confuse the installed plugin directory with the application configuration.
For a missing file, create only the sections needed for the requested features, using general defaults from the
reference. Ask for missing endpoint, model, credential source, or base voice rather than inventing them.

Inspect relevant settings through a TOML parser with credential values redacted before output. Never dump the raw
file, environment, credential command output, or an unredacted diff. Do not execute an existing `cmd:` value just
to inspect configuration; it is shell code. Store credential references, never literal tokens or keys.

### Make the requested change

- Keep edits narrow and preserve unrelated settings and comments. Do not replace a working configuration with the
  example wholesale. When replacement is necessary, validate a candidate first and preserve file permissions.
- Configure STT, LLM, TTS, and live independently. Each service has named providers (`[<service>.providers.<name>]`, active one in `provider`); `api` selects the wire protocol, not the model's upstream vendor.
  Use model IDs and voice names supplied by the user or verified for their endpoint; do not assume a catalog.
- For transform, define `[stt.transforms.<name>]` and select it with `transform` in the stt provider. Explain that transform adds an LLM request;
  omit the selection when the user wants raw transcription or the lowest latency.
  Vocabulary, names and fixed spellings ("always write Kubernetes") belong in the transform `prompt`; Habla has no
  separate vocabulary or replacement setting.
- Keep `voice` in the tts provider, even when styles exist (styles hold only `instructions`, in shared `[styles.<name>]`). A table named `default` has no automatic effect.
  Applying a style needs both `style` and an explicit compatible `instructions_mode` in the provider. Do not add a second
  LLM call to compensate for a speech model that does not support instructions.
- Preserve recording, streaming, splitting, paste, and hotkey behavior unless the user asks to change them.

Tray menu choices (each service's `provider`, the active provider's `model`, `language`, `transform`, `voice`, and `[tray] paste` / `paste_enter`) are written into `config.toml` itself;
there is no separate state file. The file is the only source of truth. `--provider NAME` on `habla listen`, `say` and `live` picks a provider for one run and is never saved; `habla listen --transform NAME` likewise picks a transform (`""`: none).

### Validate and hand back

Validate the candidate's TOML syntax without displaying its contents, then check the reference's schema rules,
named references, and mode/provider compatibility. A syntax parser alone does not validate Habla semantics.
There is no standalone `habla config --check` command. Do not start recording, speak, copy, paste, execute credential
commands, or call an API as a substitute for validation unless the user has asked for that test. Configuration
editing does not authorize installing packages or changing gateway configuration.

Report the file edited, a redacted summary, and exactly what was checked. Separate
local validation from untested credentials, model availability, voice quality, and instruction support. Habla
reloads configuration for new commands/recordings; hotkey changes may require restarting the tray, and portal
shortcuts on KDE/GNOME Wayland are managed by the desktop.

## Speak with habla say

`habla say "text"` speaks with the active `tts` provider. Facts (from `internal/say`):

- Text is ONE argument (a second one is rejected, exit 2), or stdin when there is none. Quote it.
- It blocks until the audio is fetched and played (`pw-play`, else `paplay`). Only one speech at a time: a second call exits 1 ("Already speaking").
- `habla say --stop` cuts the speech (exit 0; exit 1 if nothing is playing). `--out FILE` writes the audio instead of playing; `--provider NAME` picks a tts provider for one run, `--style NAME` a style (`--style ""`: none).
- Failures (no `[tts.providers.<name>]`: "TTS is not configured", no player, bad credential) exit 1 and are reported as desktop notifications, not on stderr.

Rules:

- Speak only when the user asked, directly or in their CLAUDE.md/AGENTS.md. Never enable speech on your own.
- One or two short plain sentences, in the user's language. Never read code, paths, logs, secrets or long output aloud.
- Do not wait for playback: run it detached, `(habla say "Done" >/dev/null 2>&1 &)`.
- If it fails, tell the user once in text and offer to configure TTS (see Configure). Do not retry in a loop.

### Opt-in setups

Install only when the user asks. Edit their file in place, keep every other key, and show what you added. Full
snippets and notes: [references/speak.md](references/speak.md).

- Instruction line in CLAUDE.md/AGENTS.md: "When you finish a task or need my input, say a one-sentence summary out loud with `habla say`."
- Claude Code hooks (`Stop`, `Notification`) in `~/.claude/settings.json` or `.claude/settings.json`.
- Codex `notify` in `~/.codex/config.toml`.
