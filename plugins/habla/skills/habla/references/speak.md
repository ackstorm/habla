# Spoken notifications (opt-in)

Never enabled by default. Install one only when the user asks. Merge into the existing file; do not overwrite
unrelated keys or hooks. Each command is detached so the hook or notifier returns at once.

## CLAUDE.md / AGENTS.md line

    When you finish a task or need my input, say a one-sentence summary out loud with `habla say`.

## Claude Code hooks

`~/.claude/settings.json` (all projects) or `.claude/settings.json` (this project). Add to the `hooks` object,
appending to an existing `Stop` or `Notification` array if present:

```json
{
  "hooks": {
    "Stop": [
      { "hooks": [ { "type": "command", "command": "(habla say 'Done' >/dev/null 2>&1 &)" } ] }
    ],
    "Notification": [
      { "hooks": [ { "type": "command", "command": "(habla say 'Claude needs your input' >/dev/null 2>&1 &)" } ] }
    ]
  }
}
```

Use the user's language for the phrases. To remove, delete these entries.

## Codex notify

`~/.codex/config.toml`, top level (before any `[table]`; `notify` may exist only once, so ask before replacing one):

```toml
notify = ["sh", "-c", "habla say 'Codex finished' >/dev/null 2>&1 &", "habla-notify"]
```

Codex appends a JSON payload as the LAST argument. With `sh -c`, that payload becomes `$1` (after `$0` =
`habla-notify`) and is ignored. Never pass it to `habla say` as text: it would be read aloud.
