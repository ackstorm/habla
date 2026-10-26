# Habla configuration reference

Schema checked against `internal/config/config.go` on 2026-10-19. Unknown keys are errors; this schema has no legacy
aliases. In particular, use `api_key`, `auth_header`, `auth_scheme`, `api` and `system_instruction`, not the
spellings from early design discussions. Connection keys live in `[<service>.providers.<name>]`, transforms in
`[stt.transforms.<name>]` and delivery styles in `[styles.<name>]`.

## Example

This is a complete example for the OpenAI API, not a universal model or voice catalog. Replace connection
choices with the user's actual provider details. It selects transform but leaves styles inactive.

```toml
[general]
max_seconds = 300
trim_below_db = -45
upload_interval_ms = 50
paste_keys = "auto"
log_level = "warn"

[tray]
hotkey = "<Super>space"
paste = false
paste_enter = false
sound = true

[llm]
provider = "openai"

[llm.providers.openai]
api = "openai"
endpoint = "https://api.openai.com/v1"
api_key = "env:OPENAI_API_KEY"
model = "gpt-4o-mini"
models = ["gpt-4o-mini"]

[stt]
provider = "openai"

[stt.providers.openai]
api = "openai"
endpoint = "https://api.openai.com/v1"
auth_header = "Authorization"
auth_scheme = "bearer"
api_key = "env:OPENAI_API_KEY"
model = "gpt-4o-mini-transcribe"
models = ["gpt-4o-mini-transcribe"]
language = ""
languages = ["en"]
transform = "clean"

[stt.transforms.clean]
prompt = """
Remove filler words and repetitions, keep final corrections, and fix punctuation and capitalization.
Preserve meaning; add nothing. When unsure, keep the original wording.
"""

[stt.transforms.punctuation]
prompt = "Fix punctuation and casing. Do not change any words."

[tts]
provider = "openai"

[tts.providers.openai]
api = "openai"
endpoint = "https://api.openai.com/v1"
api_key = "env:OPENAI_API_KEY"
model = "gpt-4o-mini-tts"
models = ["gpt-4o-mini-tts"]
voice = "alloy"
voices = ["alloy", "marin"]
format = "wav"
# instructions_mode = "instructions"
# style = "storyteller"

[live]
provider = "openai"

[live.providers.openai]
endpoint = "wss://api.openai.com/v1/realtime"
api_key = "env:OPENAI_API_KEY"
model = "gpt-realtime"
voice = "marin"
voices = ["marin", "cedar"]
# instructions_mode = "instructions"
# style = "storyteller"
# tools = false

[styles.default]
instructions = "Speak clearly in a calm, friendly tone."

[styles.storyteller]
instructions = "Read like a warm storyteller, with gentle pauses."

[styles.sevilla]
instructions = "Speak Spanish with an Andalusian accent from Seville."

[split]
enabled = true
after = 10
pause = 1

[prices]                             # USD per million tokens; used when the gateway sends no cost header
"gpt-4o-mini-transcribe" = { input = 1.25, audio = 3.0, output = 5.0 }
```

`[prices]` is optional, keyed by model name, with `input`/`audio`/`output` in USD per 1 M tokens. LiteLLM's
`x-litellm-response-cost` header wins over it; models with neither show tokens only in `habla stats`. For
`api = "elevenlabs"` the unit is characters (the reply's `character-cost`, counted as `in`): `input` is USD per 1 M characters.

## Providers

Each service (`[stt]`, `[llm]`, `[tts]`, `[live]`) is optional and holds one or more
`[<service>.providers.<name>]` tables. Names use only letters, digits, `-` and `_`. The service's `provider` key
picks the active one; with a single provider it may be left out, with several it is required and must name one of
them. The tray menu saves the choice into `provider`; `--provider NAME` overrides it for one run without writing
the file. Everything below (connection keys, `voice`, `style`, `transform`) belongs to a provider, so switching
provider switches the whole set. Without STT, dictation is not configured; without TTS, speech is not
configured; without Live, `habla live` is not configured. Without LLM, selected transform is skipped with a warning
and the transcript stays raw. Missing services do not inherit another service's connection.

## Connections and protocol

Every `[stt|llm|tts.providers.<name>]` requires `endpoint`, `api_key`, and `model`.

| Key | Meaning |
| --- | --- |
| `endpoint` | Base URL; Habla appends the operation path. Do not include `/audio/speech` or `:generateContent`. |
| `api` | `openai` (default), `gemini`, or `elevenlabs` (tts only; see Voice and styles); selects the protocol regardless of the upstream model vendor. |
| `auth_header` | Default `Authorization`; use the header required by that endpoint. |
| `auth_scheme` | `bearer` (default) adds `Bearer `; `raw` sends the credential without a prefix. |
| `api_key` | `env:VARIABLE` or `cmd:COMMAND`, with a nonempty suffix. No literal keys, empty fallback, or implicit inheritance. |
| `model` | Exact model ID exposed by that endpoint. |
| `models` | Optional list of model IDs; accepted but unused for now (the tray menu no longer lists models). |

OpenAI-compatible calls append `/audio/transcriptions`, `/chat/completions`, or `/audio/speech`, usually to a base
ending in `/v1`. Native Gemini calls append `/models/<model>:generateContent`, for example to a base ending in
`https://generativelanguage.googleapis.com/v1beta` (Google API keys use `auth_header = "x-goog-api-key"` and
`auth_scheme = "raw"`). Protocol selection is explicit, not inferred from the URL.

An environment key is referenced as `api_key = "env:HABLA_API_KEY"`. A raw-key header, when supported by the
endpoint, needs both its header name and `auth_scheme = "raw"`. Do not assume native-route authentication or model
capabilities from these examples; check current provider documentation or test only when requested.

## Dictation and transform

- `language` (in an stt provider) absent or `""` means automatic detection. `languages` is accepted but unused for
  now (the tray menu no longer lists languages). Gemini STT does not send the language hint.
- `transform` (in an stt provider) absent or `""` means no transform. `"none"` is not a sentinel; it would be a named
  definition. `habla listen --transform NAME` uses another one for one run without writing the file (`""`: none).
- `[stt.transforms.<name>]` requires a nonempty `prompt`, which may span lines with `"""`. Optional `model`
  overrides the active llm provider's `model`; endpoint and credentials always come from `[llm]`. The selected
  transform name must exist, even when LLM is not configured. Transforms are shared by all stt providers.
- There is no `transform.preset` or `transform.prompts` key in this schema. A transform's own `model` wins over the active
  llm provider's `model`.

## Voice and styles

`voice` is required in every tts provider. It is the voice used with or without a style; styles never carry a
voice. `format` is provider-dependent; native Gemini accepts only absent/empty or `"wav"` in this implementation.
Do not assume other Gemini audio encodings work. `voices` may name voices (`"Name=ID"`).

A `voices` entry is `"ID"` or `"Name=ID"` (split at the first `=`); `voice` may be a name from `voices` or an ID. The
tray lists and saves the name, and requests carry the ID.

`api = "elevenlabs"` (`habla say` only; rejected in `stt` and `llm`) calls `POST {endpoint}/text-to-speech/{voice ID}`
and plays 24 kHz WAV. `format` must be absent or `"wav"`, and `instructions_mode` must be absent (pick an accent with
the voice). The default config ships it inactive:

```toml
[tts.providers.elevenlabs]
api         = "elevenlabs"
endpoint    = "https://api.elevenlabs.io/v1"
auth_header = "xi-api-key"
auth_scheme = "raw"
api_key     = "env:ELEVENLABS_API_KEY"
model       = "eleven_multilingual_v2"
voice       = "George"
voices      = ["George=JBFqnCBsd6RMkjVDRZzb", "Alice=Xb7hH8MSUJpSbSDYk0k2"]
```

Each `[styles.<name>]` has a nonempty `instructions` (may span lines) and is shared by tts and live providers.
A provider's `style` absent or `""` means no style, including when a style named `default` is defined. A selected
name must exist. No `instructions_mode` means no instructions are applied; a selected inactive style produces a
warning. Merely defining an unselected style does not produce a warning in the current implementation.
`habla say` and `habla live` take `--style NAME` to use another style for one run without writing the file;
`--style ""` means no style, as `style = ""` does.

| `tts` provider `instructions_mode` | Protocol | Delivery |
| --- | --- | --- |
| absent or `""` | Either | Base voice, plain text; styles disabled. |
| `inline` | Either | Instructions and text wrapped in `<systemInstructions>` and `<text>` inside the input text. |
| `instructions` | OpenAI only | Separate speech request `instructions` field. |
| `system_instruction` | Gemini only | Native `systemInstruction` field. |

Modes describe request construction, not verified support by every model. In particular, a successful response
does not prove a model obeyed instructions. Never claim accent quality without listening or infer instruction
support from an HTTP 200 alone. Habla does not rewrite speech with an LLM as a fallback.

## General settings, state, and validation

### Experimental live conversation

`[live]` is optional and its providers have their own connection. Each requires `endpoint`, `model`, and
`api_key`:

```toml
[live]
provider = "gateway"

[live.providers.gateway]
endpoint = "wss://gateway.example/v1/realtime"
model = "gemini.gemini-3.8-live"
api_key = "env:HABLA_API_KEY"
voice = "marin"
voices = ["marin", "cedar"]
instructions_mode = "instructions" # inline | instructions; unset: voice and style disabled
# style = "storyteller"        # existing [styles.storyteller]
# tools = false                # unset: file tools (a LiteLLM Gemini bridge needs gemini_live_defer_setup)
```

`endpoint` is the full `wss:` URL without query, fragment or embedded credentials; Habla adds the `model` query
parameter. Authentication is fixed to `Authorization: Bearer`, using the existing `env:` / `cmd:` sources.
Optional `style` names a shared `[styles.<name>]`; the provider owns its `voice`.
Without `instructions_mode`, live voice and style are disabled (a selected inactive voice or style warns). `inline`
sends only the style's delivery instructions in the initial conversation item and leaves the native voice unchanged.
`instructions` sends native session instructions plus the provider's `voice` and requires
confirmation in `session.updated`; this supports OpenAI Realtime. Use `inline` when the Gemini gateway ignores
native setup updates. `system_instruction` is rejected for live: it is a Gemini batch field, not a field in this
WebSocket protocol. Do not promise accent quality or assume TTS voice names are valid for the realtime model.
There are no `api`, `models` or audio settings in a live provider: OpenAI and Gemini models both use the realtime
event protocol, Gemini through the gateway's bridge. `tools` (default true) gives the model function tools: `habla live`
lets it create text files in the folder it runs in, and edit or delete only the files it created in that
conversation; `tools = false` turns this off for the provider, `--no-tools` for a run. A model that takes no tools
ignores them. A LiteLLM Gemini bridge passes tools
only with `litellm_settings: gemini_live_defer_setup: true`; without it the model says it has no tools. Context is
supplied with `habla live --context FILE` (UTF-8, at most 1 MiB), read once before connecting and sent to the
service as a conversation item. The service may acknowledge it before the first spoken turn. This command uses
the terminal, streams 24 kHz mono PCM16, pauses microphone transmission during responses, and ends on Ctrl+C
or `general.max_seconds`. Transcript text is saved incrementally by default under the config directory as
`live_YYYYMMDD_HHMMSS.txt` (local time, permissions `0600`, no overwrite); partial text remains after termination.
`--no-output` disables file saving; `--output-file FILE` chooses another file. The flags are mutually exclusive;
terminal output remains enabled. Saved text contains provider-supplied conversation transcripts, not raw context,
style settings, credentials or status messages. Context and transcript content must not enter diagnostic logs.
Do not execute credential commands
or make live requests without the user's authorization. Recorded-audio validation does not establish physical
microphone or speaker behavior.

### Existing general settings

Absent `[general]` keys use the example's values. `paste_keys` accepts `auto`, `ctrl+v`, or `ctrl+shift+v`;
`log_level` accepts `error`, `warn`, `info`, or `debug`. Preserve existing recording/hotkey choices.
`[tray]` holds `hotkey`, `paste`, `paste_enter` (Enter after pasting; needs `paste`) and `sound` (default `true`: a
short tone as the hotkey starts and stops recording, and as Enter sends). Only `habla tray` reads
them; `habla listen` takes `--paste` / `--paste_enter`. Old `[general] hotkey` / `paste` are errors ("moved to
tray.KEY"): move those lines into `[tray]`.
`upload_interval_ms` is an integer from 50 to 1000 (default 50): the audio upload polling interval during
dictation. Stopping flushes the remaining audio immediately.
`[split]` is enabled by default: `after = 10`, `pause = 1`. Audio uploads while recording; a one-second silence
ends a segment once it has lasted at least ten seconds. The next segment starts counting when speech resumes.
The joined transcript is transformed once after recording ends. `enabled = false` opts out, and
`trim_below_db = 0` disables silence-based splitting. When enabled, `after` and `pause` must be positive finite
numbers; thresholds longer than `general.max_seconds` simply cannot be reached during a recording.
Existing configuration files are not rewritten: explicit split settings remain in effect, and missing keys
inherit the defaults.

The configuration uses XDG paths and is the only source of truth for preferences. The tray saves the stt and llm `provider`, writes the active stt
provider's `transform`, and `tray.paste` / `tray.paste_enter`, directly into that same TOML file, preserving
unrelated keys and comments. There is no separate tray-choice state file or override to clear. Empty transform means
raw transcription; empty language means Auto. Runtime recording/delivery state is separate from configuration.

Local TOML parsing must not echo the file or exception snippets that could contain secrets. Check required fields,
unknown keys, allowed values, selected names, and protocol/mode compatibility separately. The repository's config
tests exercise its parser, but running tests does not validate the user's actual file or contact the gateway.
