#!/bin/sh
# Habla installer: downloads the release binary, verifies its checksum, installs it in ~/.local/bin.
# HABLA_RELEASE_URL overrides the download location, HABLA_INSTALL_DIR the install directory.
# Everything runs inside main, called on the last line: a truncated `curl | sh` download runs nothing.
set -eu

die() { echo "habla install: $*" >&2; exit 1; }
warn() { echo "warning: $*" >&2; }
have() { command -v "$1" >/dev/null 2>&1; }

main() {
	URL=${HABLA_RELEASE_URL:-https://github.com/ackstorm/habla/releases/latest/download}
	DIR=${HABLA_INSTALL_DIR:-$HOME/.local/bin}

	case $(uname -m) in
	x86_64 | amd64) arch=amd64 ;;
	aarch64 | arm64) arch=arm64 ;;
	*) die "unsupported architecture $(uname -m)" ;;
	esac

	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT
	trap 'exit 1' INT TERM

	bin=habla-linux-$arch
	echo "Downloading $bin"
	fetch "$bin" || die "download of $bin failed"
	fetch SHA256SUMS || die "download of SHA256SUMS failed"

	want=$(grep " \**$bin\$" "$tmp/SHA256SUMS" | head -n 1) || true
	[ -n "$want" ] || die "no checksum for $bin in SHA256SUMS"
	(cd "$tmp" && echo "$want" | sha256sum -c - >/dev/null 2>&1) || die "checksum mismatch for $bin; nothing installed"

	mkdir -p "$DIR"
	chmod 0755 "$tmp/$bin"
	mv -f "$tmp/$bin" "$DIR/habla.new"
	mv -f "$DIR/habla.new" "$DIR/habla"
	echo "Installed $DIR/habla ($("$DIR/habla" --version 2>/dev/null || echo unknown version))"

	if ! "$DIR/habla" tray --install-autostart </dev/null >/dev/null 2>&1; then
		warn "could not set up the tray autostart; run 'habla tray --install-autostart' once the tray is available"
	fi

	case ":$PATH:" in
	*":$DIR:"*) ;;
	*) warn "$DIR is not on your PATH; add it to your shell profile" ;;
	esac

	check_deps

	if grep -q '^[[:space:]]*active[[:space:]]*=' "$HOME/.config/habla/config.toml" 2>/dev/null; then
		warn "$HOME/.config/habla/config.toml uses the v2 key 'active': rename it to 'preset' (and the value 'off' to 'none')"
	fi

	cat <<'N'

Next steps:
  habla listen             dictate in a terminal (Enter stops and copies; Esc or Ctrl+C discards)
  habla tray               tray icon, hold Super+Space to dictate
Log out and in (or run habla tray) to start the tray. Docs: https://github.com/ackstorm/habla
N
}

fetch() { # fetch NAME: download $URL/NAME into $tmp/NAME
	if have curl; then
		curl -fsSL "$URL/$1" -o "$tmp/$1"
	elif have wget; then
		wget -qO "$tmp/$1" "$URL/$1"
	else
		die "need curl or wget"
	fi
}

# Runtime dependencies: warn only, by session type.
check_deps() {
	have pw-record || have parecord || warn "no recorder: sudo apt install pipewire-bin (or pulseaudio-utils)"
	if [ "${XDG_SESSION_TYPE:-}" = wayland ] || [ -n "${WAYLAND_DISPLAY:-}" ]; then
		have wl-copy || warn "no clipboard tool: sudo apt install wl-clipboard"
		have wtype || have ydotool || warn "no paste tool: sudo apt install wtype (sway, Hyprland) or ydotool (KDE, GNOME); only needed for --paste"
	else
		have xclip || warn "no clipboard tool: sudo apt install xclip"
	fi
	ldc=
	if have ldconfig; then ldc=ldconfig; elif [ -x /sbin/ldconfig ]; then ldc=/sbin/ldconfig; fi
	if [ -n "$ldc" ]; then
		"$ldc" -p 2>/dev/null | grep -q libayatana-appindicator3 || warn "tray icon library missing: sudo apt install libayatana-appindicator3-1"
		"$ldc" -p 2>/dev/null | grep -q libgtk-layer-shell || echo "optional: sudo apt install libgtk-layer-shell0 (recording pill on KDE, sway, Hyprland)"
	fi
	[ -n "${OPENAI_API_KEY:-}" ] || warn "no API key: set OPENAI_API_KEY or change api_key in ~/.config/habla/config.toml"
}

main "$@"
