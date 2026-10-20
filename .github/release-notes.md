Habla @VERSION@: push-to-dictate for the Linux desktop through OpenAI-compatible, Gemini or ElevenLabs (TTS) speech APIs. One self-contained binary (no cgo; needs glibc).

## Changes

@CHANGES@

Full history: [CHANGELOG.md](https://github.com/ackstorm/habla/blob/v@VERSION@/CHANGELOG.md).

## Install

    curl -fsSL https://github.com/ackstorm/habla/releases/latest/download/install.sh | sh

It installs `~/.local/bin/habla` (checksum verified) and sets up the tray autostart. Run it again to upgrade; a running tray keeps the old binary until restarted (Quit from its menu, then run `habla tray`, or log out and in).

## Quick usage

- `habla listen`: dictate in a terminal; the text lands on the clipboard (`--paste` also pastes it).
- `habla tray`: tray icon; hold `Super+Space` to dictate (X11, KDE and GNOME via the portal; sway and Hyprland bindings in the README).
- `habla say "Hello, how are you?"`: speak text.
- `habla live --context notes.txt`: discuss a text file in an experimental spoken conversation.
- `habla stats`: usage, cost and latency.

## Dependencies

A recorder (`pipewire-bin` or `pulseaudio-utils`), a clipboard tool (`xclip` or `wl-clipboard`), `libayatana-appindicator3-1` for the tray,
optionally `libgtk-layer-shell0` (recording pill on Wayland), `wtype` or `ydotool` (Wayland paste),
and an API key (`$OPENAI_API_KEY` by default). The installer warns about what is missing.
