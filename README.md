# Habla

**Hold a key, speak, release: the transcript lands on the clipboard.**

![Dictation: listening, transcribing and refining text](docs/images/recording-pill.gif)

Habla talks to OpenAI-compatible, Gemini or ElevenLabs (TTS) speech APIs and works in any language the provider
supports.

- `habla tray`: hold-to-talk dictation to the clipboard, or pasted into the focused window
- `habla listen`: speech to text from a terminal or your own key bindings, with optional LLM transforms
- `habla say`: text to speech, with optional styles
- `habla live` (experimental): realtime spoken conversation
- `habla stats`: cost and latency per service

**Features:**

Optional LLM transforms: remove filler words, fix punctuation, translate, write commit messages, ...

Optional styles: accent, tone and delivery for `habla say` and `habla live`

One static binary (no cgo; needs glibc)

## Quick start

1. Install:

       curl -fsSL https://github.com/ackstorm/habla/releases/latest/download/install.sh | sh

2. Put your key in the shell profile (`~/.profile`, `~/.zshenv`, ...) and log in again:

       export OPENAI_API_KEY=...

   For Gemini, export `GEMINI_API_KEY` instead and switch the `stt`, `llm` and `tts` `provider` to `"gemini"` in
   `~/.config/habla/config.toml` (written on first run).
3. Run `habla tray`, then hold `Super+Space` and speak.

## Install

The script downloads `habla-linux-<arch>` (amd64 or arm64), checks it against `SHA256SUMS`, installs
`~/.local/bin/habla` and runs `habla tray --install-autostart` (autostart entry, tray icons, the portal's desktop
entry). Run it again to upgrade; a running tray keeps the old binary until restarted (Quit from its menu, then run
`habla tray`, or log out and in). `HABLA_INSTALL_DIR` changes the install directory. Make sure `~/.local/bin` is on
your `PATH`.

### Dependencies

The installer warns about what is missing (Debian/Ubuntu package names):

| Need | Package |
|---|---|
| Recorder (and `habla say` playback) | `pipewire-bin` (`pw-record`, `pw-play`) or `pulseaudio-utils` (`parecord`, `paplay`) |
| Clipboard, X11 | `xclip` (paste and terminal detection are built in) |
| Clipboard, Wayland | `wl-clipboard`; paste needs `wtype` (sway, Hyprland) or `ydotool` 1.x with `ydotoold` running (KDE, GNOME) |
| Tray icon | `libayatana-appindicator3-1` |
| Recording pill on KDE, sway, Hyprland | `libgtk-layer-shell0` (optional) |

## Usage

    habla listen              # in a terminal: Enter stops and copies; Esc or Ctrl+C discards
    habla listen --paste      # ...and paste it into the focused window
    habla listen --paste_enter  # ...paste it, then press Enter
    habla listen --transform english  # ...with another [stt.transforms.*] for this run ("": none)
    habla tray                # tray icon + hold-to-talk hotkey (Super+Space)
    habla say "Hello"         # speak
    habla live --context notes.txt  # experimental spoken conversation about a text file
    habla stats               # cost and latency
    habla --version

`habla listen` also takes `--start`, `--stop`, `--toggle`, `--cancel`, `--retry`, `--send` and `--status`, for key bindings.
`listen`, `say` and `live` take `--provider NAME` to use another configured provider for one run; `say` and `live`
also take `--style NAME` (a `[styles.*]` name, or `""` for none) to override the provider's style for one run, and
`listen` takes `--transform NAME` (a `[stt.transforms.*]` name, or `""` for none) to override its transform.

## Configuration

`~/.config/habla/config.toml` is yours: Habla writes it on first run only and re-reads it at each recording. Invalid
files stop the command with a line-numbered error; unknown keys are errors. `[general]` holds the recording
limit, silence threshold, upload interval, paste keys and log level; `[tray]` holds `hotkey`, `paste`,
`paste_enter` and `sound` (a rising two-note tone as the hotkey starts recording, a falling one as it stops, so a
missed press is heard at once, and a "pip" when Enter sends; `false` = silent), which only `habla tray` reads
(`habla listen` takes `--paste` / `--paste_enter`).

```toml
[stt]
provider = "openai"            # the active one

[stt.providers.openai]
endpoint = "https://api.openai.com/v1"
api_key = "env:OPENAI_API_KEY" # env:VARIABLE or cmd:COMMAND; literal keys are rejected
model = "gpt-4o-mini-transcribe"
transform = "clean"

[stt.transforms.clean]
prompt = """Remove filler words and false starts, keep the final correction.
Fix punctuation and capitalization. Do not change the meaning."""

[styles.warm]
instructions = "Speak in a warm, friendly tone."
```

The generated file ships `openai` and `gemini` providers for `llm`, `stt` and `tts`, an inactive `elevenlabs` tts
provider, an `openai` live provider, and the transforms `clean` (the default), `punctuation`, `english` and `commit`.

[`examples/config.toml`](examples/config.toml) is a complete annotated config that mixes providers: Gemini
transcribes, OpenAI transforms and ElevenLabs speaks, with an alternative for each, styles, `[split]` and `[prices]`.

### Providers

`[stt]`, `[llm]`, `[tts]` and `[live]` each hold named `[<service>.providers.<name>]` tables; `provider` in the
service table picks the active one (optional when there is only one). Each service is optional: without `[llm]` there
is no transform, without `[tts]` `habla say` exits with *TTS is not configured*.

| Key | Where | Meaning |
|---|---|---|
| `endpoint`, `api_key`, `model` | all | `api_key` is `env:VARIABLE` or `cmd:COMMAND` |
| `api` | stt, llm, tts | `openai` (default), `gemini`, or `elevenlabs` (tts only) |
| `auth_header`, `auth_scheme` | stt, llm, tts | header name; `bearer` or `raw` |
| `models` | stt, llm, tts | accepted, unused for now (the tray no longer lists models) |
| `voice`, `voices` | tts, live | base voice; `Name=ID` names for `voice` |
| `language`, `languages`, `transform` | stt | `""` = auto; accepted, unused for now; a `[stt.transforms.*]` name |
| `instructions_mode`, `style` | tts, live | how a shared `[styles.<name>]` is applied (see Speak) |

Transforms are `[stt.transforms.<name>]` tables with a required `prompt` (multi-line `"""` strings allowed) and an
optional model override. The selected transform uses the active `[llm]` provider; without one, transforms are skipped and the
raw text is copied. Vocabulary and fixed spellings ("always write Kubernetes") belong in the prompt.

**Gemini:** `endpoint = "https://generativelanguage.googleapis.com/v1beta"`, `api = "gemini"`,
`auth_header = "x-goog-api-key"`, `auth_scheme = "raw"` and a Gemini model (for example `gemini-3.5-transcribe` or
`gemini-3.8-flash`). Transcription detects the language; `language` is not sent. Transforms disable thinking to reduce
latency.

**ElevenLabs** speaks for `habla say` only (`api = "elevenlabs"` in a `[tts.providers.*]` table; the default config has
an inactive entry reading `$ELEVENLABS_API_KEY`). It takes no `instructions_mode` (pick an accent with the voice) and is
rejected in `stt` and `llm`. A `voices` list may use `"Name=ID"` entries: `voice` can be the name or the ID, and the
request carries the ID.

**Tray choices** (only what dictation uses: *Transform*, *STT provider* and *LLM provider*, which show the active
provider even when it is the only one, *Also paste automatically*, *And press Enter*) are written into
`config.toml`, keeping its other lines and comments; *None* writes `transform = ""`. The tray reloads the file whenever
it is saved, so *Edit config…* applies at once (except `hotkey`, which needs a tray restart).

**Upload interval:** `[general] upload_interval_ms` sets how often audio is sent while recording: 50 ms by default,
50–1000 ms. Stopping flushes the rest at once.

## Desktop integration

### Tray

    habla tray                        # tray icon, and the hold-to-talk hotkey on X11, KDE and GNOME
    habla tray --foreground           # the same, attached to the terminal (autostart runs this)
    habla tray --install-autostart    # start it at login (the installer already did)

`habla tray` starts the tray in the background, in a session of its own, and returns once its icon is up; with a tray
already running it says so and exits 1. Its output goes to the log. Upgrading with the installer restarts it.

The icon shows the state: a microphone when idle, a red square while recording (with the elapsed time, where the
panel shows labels), amber while transcribing, a red microphone after a failed transcription. Middle-click starts or
stops a dictation; right-click opens the menu. Only one tray runs at a time. *Quit* leaves a recording running.

While recording, a slim white bar on the monitor of the active window shows **Listening**, a pulsing rose dot,
and a lavender waveform with 28 audio-driven peak bars in the left 30% of the bar. The text, timer and ESC share
the remaining 70%. Low microphone input (at or below -40 dBFS)
settles to a one-pixel line; this threshold affects only the display. Bar heights make quiet speech more visible;
colours indicate speech intensity: amber from -20 dBFS and red from -12 dBFS. Red does not mean the audio is clipping.
The recording duration sits at the right, before the cancellation
hint, and freezes when recording ends. **Transcribing** uses aqua activity and a rotating spinner; **Refining text**
uses gold activity and a shimmering star. Both activity bars bounce every 0.65 seconds. Escape remains visible in every phase where it can cancel,
beside Enter (*cancel / send*) while transcribing or refining; after Enter the key shows a tick (*cancel / queued*) and
the subtitle *Will paste and send (keep this window focused)*; recording
started from the tray or through a Wayland shortcut keeps the tray instructions. Clicks pass through the bar and it
never takes focus. The last phase stays visible until copying and pasting finish. A copy failure leaves an error
bar until the next dictation; the notification keeps the text. On Wayland the bar needs layer-shell (KDE Plasma,
sway, Hyprland) and `libgtk-layer-shell0`; on GNOME under Wayland a *Listening* notification takes its place.

GNOME shows tray icons only with the *AppIndicator and KStatusNotifierItem Support* extension (on by default on
Ubuntu, not on Debian or Fedora). Without it dictation keeps working with the hotkey. On sway and Hyprland start the
tray from the compositor config instead of autostart: `exec habla tray --foreground` (sway),
`exec-once = habla tray --foreground` (Hyprland).

### Hotkey

Hold the hotkey to record, release to transcribe. Default `Super+Space` (`hotkey = "<Super>space"`; modifiers
`<Control>`, `<Alt>`, `<Shift>`, `<Super>`, then a letter, a digit, `F1` to `F12`, `space`, `Return`, `Escape` or
`Tab`).

Press again while Habla is still transcribing or transforming (amber icon) to continue the same dictation: it drops a
running transform, waits for the last part, transforms the joined text once and copies it. Esc while transcribing or
transforming discards the whole dictation, and Enter pastes it and presses Enter once it is ready, whatever the paste
setting; Enter while still holding the hotkey stops and sends at once. On KDE and GNOME Wayland these are the
hotkey's modifiers plus Enter or Esc (`Super+Enter`, `Super+Esc`: keep Super down after letting go of Space); on sway
and Hyprland bind `habla listen --send` / `--cancel` (below). After Enter on X11, Habla holds the keyboard and mouse
until it pastes (at most 60 s), so the focus stays on the window you dictated for; Esc still cancels.

Dictation splits automatically by default (`[split] enabled = true`, `after = 10`, `pause = 1`). Audio uploads
while you speak; a one-second pause ends a segment once it has lasted at least ten seconds. The timer restarts
when speech resumes, and the joined transcript is transformed once when you finish. Shorter recordings finish
normally without splitting. These are silence-triggered cuts, not fixed ten-second chunks.

Set `[split] enabled = false` to opt out. Pauses are detected with `trim_below_db`, so `trim_below_db = 0` also
disables splitting. Existing configuration files are not rewritten: explicit values stay in effect, including
`enabled = false`; missing keys inherit the defaults.

**X11, any desktop:** `habla tray` grabs the key itself; Esc while holding it discards the recording. GNOME uses
`Super+Space` for input sources and KDE may use it for KRunner: pick another key there. If another app holds the key,
the tray says so; middle-click still works.

**sway** (`~/.config/sway/config`; replaces sway's default `$mod+space` binding):

    bindsym $mod+space exec habla listen --start    # add --paste or --paste_enter to paste
    bindsym --release $mod+space exec habla listen --stop
    bindsym $mod+Return exec habla listen --send    # replaces sway's default terminal binding
    bindsym $mod+Escape exec habla listen --cancel

**Hyprland** (`~/.config/hypr/hyprland.conf`):

    bind  = SUPER, space, exec, habla listen --start
    bindr = SUPER, space, exec, habla listen --stop
    bind  = SUPER, Return, exec, habla listen --send
    bind  = SUPER, Escape, exec, habla listen --cancel

**KDE Plasma 5.27+ and GNOME 48+ on Wayland:** `habla tray` asks the desktop through the XDG GlobalShortcuts portal.
The first time, a dialog proposes `hotkey`, plus its modifiers with Enter (send) and Esc (discard); you may pick other
keys. A hotkey without modifiers gets no send or discard shortcut. Afterwards change it in *System Settings →
Shortcuts → Habla* (KDE) or *Settings → Apps → Habla* (GNOME). The portal identifies Habla by
`~/.local/share/applications/io.github.ackstorm.Habla.desktop`: leave it there.

Without the portal (older desktop) or if you decline the dialog there is no hold-to-talk. Bind a shortcut to
`habla listen --toggle` (press once to start, again to stop): KDE *System Settings → Shortcuts → Add New → Command or
Script…*, GNOME *Settings → Keyboard → Custom Shortcuts → +*. If a shortcut does nothing, use the full path from
`command -v habla`. Discard with *Discard dictation* in the tray menu or `habla listen --cancel`.

### Paste

Habla always copies the text. Three modes:

| Tray menu | `[tray]` | `habla listen` |
|---|---|---|
| (both off) | `paste = false` | (no flag) |
| *Also paste automatically* | `paste = true` | `--paste` |
| *And press Enter* (needs paste) | `paste = true`, `paste_enter = true` | `--paste_enter` |

Pasting happens in whatever window is focused when the transcript arrives. Enter follows 100 ms after the paste, with
the same tool, and does what Enter does there: it may submit a message, insert a newline, or run a command in a
terminal. Key bindings that run `habla listen --start` / `--toggle` paste only with one of the flags.

| Session | Packages | Keystroke |
|---|---|---|
| X11 | `xclip` | built in (XTEST), no extra tool |
| Wayland, sway / Hyprland | `wl-clipboard`, `wtype` | `wtype -M ctrl v -m ctrl` |
| Wayland, KDE / GNOME | `wl-clipboard`, `ydotool` 1.x | `ydotool key 29:1 47:1 47:0 29:0` |

On Wayland `wtype` is tried first, then `ydotool` (needs `ydotoold` and `/dev/uinput`; 0.1.x is unsupported). If
nothing can paste, the text stays on the clipboard and the notification says *Copied: press Ctrl+V to paste*.

`paste_keys = "auto"` sends `Ctrl+Shift+V` on X11 when the focused window's `WM_CLASS` is a known terminal
(gnome-terminal, mate-terminal, konsole, alacritty, kitty, xterm, anything containing `terminal`, ...) and `Ctrl+V`
otherwise; on Wayland always `Ctrl+V`. Set `ctrl+v` or `ctrl+shift+v` to force one.

## Speak

    habla say "Hello, how are you?"       # speak it (pw-play, else paplay)
    echo "Good morning" | habla say       # text from stdin
    habla say --out greeting.wav "Hello" # write the audio instead of playing it
    habla say --style sevilla "Hola"      # another [styles.*] for this run ("": none)
    habla say --stop                      # stop what is playing

`habla say` uses the active `[tts.providers.<name>]` for its model, base voice and format. To change the delivery,
select a shared `[styles.<name>]` with `style = "name"` in the provider and set `instructions_mode` to `inline`,
`instructions` or `system_instruction`; the instructions travel in the single speech request (there is no text rewrite
request). Use `inline` for models that ignore the separate `instructions` field. Without `instructions_mode` a selected
style is not applied and `habla say` warns once. An unset style speaks the original text.

## Live (experimental)

`habla live` opens one realtime WebSocket conversation in a terminal. Optionally give it a UTF-8 text file to discuss
(up to 1 MiB):

```sh
habla live --context ./notes.txt
habla live --no-output                    # terminal transcript only
habla live --output-file ./discussion.txt # choose where the transcript is saved
habla live --no-tools                     # no file tools for this run
```

The default config ships an `openai` live provider. A gateway with a realtime bridge works the same way:

```toml
[live]
provider = "gateway"

[live.providers.gateway]
endpoint = "wss://gateway.example/v1/realtime"
model = "gemini.gemini-3.8-live"
api_key = "env:HABLA_API_KEY"  # or cmd:COMMAND; never a literal credential
# instructions_mode = "inline" # unset: styles disabled; inline | instructions
# style = "warm"               # reuses [styles.warm]
# tools = false                # unset: file tools (below)
# max_seconds = 3600           # the session's limit
# barge_in = true              # speaking interrupts the reply (headphones or echo cancellation)
```

The endpoint is the full WebSocket URL; Live uses `Authorization: Bearer` and the gateway's realtime event protocol,
and does not inherit STT/TTS settings. The context file is read once before connecting and sent as conversation
context, and an inline style goes with it. Both are sent before you speak, and the session then waits silently for you.

Conversation text appears in the terminal and is saved incrementally to `~/.config/habla/live_YYYYMMDD_HHMMSS.txt`
(`$XDG_CONFIG_HOME/habla` when set; mode `0600`, never overwrites a file, keeps partial text on cancellation or
failure). `--no-output` disables saving; `--output-file FILE` selects another path (mutually exclusive). The transcript
excludes context, style settings, credentials and status messages; if it cannot be written the session stops.

By default (`tools = false` in the live provider turns it off; a model that takes no tools ignores them) the
model can create text files in the folder where you run `habla live`, and edit or delete the files it created in
that conversation; nothing else. Ask out loud: *"write a summary of what we said to summaries/today.md"*. It cannot
read, change or delete a file that was there before, and paths cannot leave the folder (links included). Each call
appears in the transcript as a `Tool:` line (the tool and path, never the content). `--no-tools` turns them off for
a run. OpenAI and Gemini models take the same tools, as both speak the realtime event protocol (Gemini through the
gateway's bridge). Tools go in the first `session.update`: a LiteLLM Gemini bridge needs
`litellm_settings: gemini_live_defer_setup: true`, or the model answers that it has no tools.

A shared `[styles.*]` can be used with `style` and `instructions_mode`, independent of the TTS selection. `inline`
puts the style's delivery instructions in the opening conversation message and leaves the service's native voice
unchanged: use it for gateways that do not accept native session instructions (for example a LiteLLM Gemini realtime
bridge). `instructions` sends native session instructions and the provider's `voice` before any audio; OpenAI Realtime
supports it, and Habla requires the server to confirm. Voice names must be valid for the realtime model.
`system_instruction` is rejected for `[live]`.

Audio is raw 24 kHz mono PCM16 through `pw-record`/`pw-play`, falling back to `parecord`/`paplay`. By default turns
are taken: microphone input is discarded while the assistant responds, so wait for *Listening…*. With `barge_in = true`
the microphone stays open and speaking interrupts the reply: the server's voice detection cancels it and Habla stops
playback at once. Use headphones or echo cancellation (PipeWire's `libpipewire-module-echo-cancel`), or the assistant
hears itself and stops; the provider must send `input_audio_buffer.speech_started` (OpenAI does; check a Gemini bridge
before relying on it). Limitations: one live session at a time (concurrent dictation or `habla say` is unsupported), no tray controls,
hotkeys, reconnects or saved recordings. Ctrl+C ends capture and playback at once; the live provider's `max_seconds`
bounds the session (3600 s, 60 minutes, by default; `general.max_seconds` limits dictation only). Compatibility with
every realtime provider is not promised.

## Stats & logs

    habla stats                  # cost by service for today / week / month / total, mean per query, latency
    habla stats --by day         # history per day (or week, month)
    habla stats --json           # the same, machine-readable

Every dictation and `habla say` appends one line to `~/.local/state/habla/stats.jsonl`: durations, token counts, cost
and character/word counts, never the text. Cost comes from the gateway's `x-litellm-response-cost` header, else from
`[prices]` in `config.toml`; models with neither show tokens only. ElevenLabs bills characters: the reply's
`character-cost` is counted as `in`, and `[prices]."<model>".input` is USD per million characters.

Every habla process appends to `~/.local/state/habla/habla.log` (`$XDG_STATE_HOME/habla`) and also prints to the
terminal when it runs in one. Over 1 MB it moves to `habla.log.1`.

    tail -f ~/.local/state/habla/habla.log
    habla listen --log debug     # this run only; the tray passes it on: habla tray --log debug

`log_level` is `warn` (default: fallbacks such as a failed stream retried as a file upload), `error`, `info` (one line
per step with its duration) or `debug` (also every audio chunk, the silence trim, each hotkey hold); `--log LEVEL`
overrides it for one run. Each entry carries `[delta_ms=N]`, the milliseconds since the previous entry in that process
(0 for the first). The log never holds the API key or what you dictate: only lengths.

## Claude Code and Codex plugin

The shared [`habla` skill](plugins/habla/skills/habla/SKILL.md) has two parts: it edits `config.toml` for you
(providers, credentials, models, voices, transforms and styles; it does not run credential commands or make live
requests unless asked), and it lets the agent speak to you through `habla say`.

```sh
# Claude Code
claude plugin marketplace add ackstorm/habla
claude plugin install habla@habla

# Codex
codex plugin marketplace add ackstorm/habla
codex plugin add habla@habla
```

In a new session ask: "Use the habla skill to configure Habla's STT model and TTS voice." or "Use the habla skill to
tell me out loud when you finish." Claude Code also exposes `/habla:habla`; Codex can invoke `$habla`. From a checkout,
`claude --plugin-dir ./plugins/habla` loads it for one session.

Spoken notifications are opt-in and off by default. Ask the skill to set them up: a line in your CLAUDE.md/AGENTS.md,
Claude Code hooks, or Codex `notify`.

## Uninstall

    pkill -f 'habla tray'
    rm -f ~/.local/bin/habla ~/.config/autostart/habla.desktop ~/.local/share/applications/io.github.ackstorm.Habla.desktop
    rm -f ~/.local/share/icons/hicolor/scalable/status/habla-{idle,recording,busy,failed}-symbolic.svg
    rm -r ~/.config/habla ~/.local/state/habla     # optional: config and state

## Building from source

Go 1.25, no cgo:

    CGO_ENABLED=0 go build -ldflags "-X main.version=dev" -o habla .
    go vet ./... && go test ./...
    HABLA_GUI=1 go test ./internal/gtk/ ./internal/tray/    # needs a display; see .github/workflows/ci.yml

Releases: push a `v*` tag; `.github/workflows/release.yml` builds amd64 and arm64, writes `SHA256SUMS` and publishes
them with `install.sh`.

## License

[MIT](LICENSE)
