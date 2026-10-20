// Package desktop has the desktop side effects: the clipboard, paste and notifications.
package desktop

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"
	"golang.org/x/sys/unix"
)

// Wayland reports a Wayland session ($WAYLAND_DISPLAY wins over a "tty" $XDG_SESSION_TYPE).
func Wayland() bool {
	return os.Getenv("WAYLAND_DISPLAY") != "" || os.Getenv("XDG_SESSION_TYPE") == "wayland"
}

// Copy puts text on the CLIPBOARD selection with wl-copy (Wayland) or xclip (X11).
func Copy(text string) error {
	name, args, pkg := "xclip", []string{"-selection", "clipboard"}, "xclip"
	if Wayland() {
		name, args, pkg = "wl-copy", nil, "wl-clipboard"
	}
	if _, err := exec.LookPath(name); err != nil {
		return fmt.Errorf("%s is not installed (package %s)", name, pkg)
	}
	// Stdout and Stderr stay nil (/dev/null): both tools fork to keep serving the selection, and a pipe held by
	// that child would never reach EOF.
	start := time.Now()
	if err := run(name, args, strings.NewReader(text)); err != nil {
		return errors.New(name + " failed")
	}
	slog.Info("copy", "tool", name, "ms", time.Since(start).Milliseconds())
	return nil
}

var (
	wtypeKeys = map[string][]string{
		"ctrl+v":       {"-M", "ctrl", "v", "-m", "ctrl"},
		"ctrl+shift+v": {"-M", "ctrl", "-M", "shift", "v", "-m", "shift", "-m", "ctrl"},
	}
	ydotoolKeys = map[string][]string{ // evdev keycodes: 29 left Ctrl, 42 left Shift, 47 V, 28 Enter
		"ctrl+v":       {"29:1", "47:1", "47:0", "29:0"},
		"ctrl+shift+v": {"29:1", "42:1", "47:1", "47:0", "42:0", "29:0"},
		"enter":        {"28:1", "28:0"},
	}
	wtypeEnter = []string{"-k", "Return"}
)

// enterDelay lets the application handle the paste before Enter.
// ponytail: a fixed 100 ms; a slow app may need longer, and there is no portable completion signal.
const enterDelay = 100 * time.Millisecond

// Paste sends the paste chord to the focused window, then Enter when enter is set. X11 uses XTEST; Wayland tries
// wtype, then ydotool 1.x, and the first that works wins (wtype fails on KDE and GNOME).
func Paste(pasteKeys string, enter bool) error {
	start := time.Now()
	if !Wayland() {
		err := pasteX11(pasteKeys, enter)
		if err == nil {
			slog.Info("paste", "tool", "xtest", "ms", time.Since(start).Milliseconds())
		}
		return err
	}
	keys := pasteKeys
	if keys == "auto" {
		keys = "ctrl+v" // Wayland gives no portable way to read the focused app
	}
	var cmds [][]string
	if _, err := exec.LookPath("wtype"); err == nil { // first: ydotool sends raw keycodes, which depend on the layout
		cmds = append(cmds, append([]string{"wtype"}, wtypeKeys[keys]...), append([]string{"wtype"}, wtypeEnter...))
	}
	if _, err := exec.LookPath("ydotool"); err == nil {
		if run("ydotool", []string{"help"}, nil) == nil { // exits 0 on 1.x, 1 on 0.1.x; needs no ydotoold
			cmds = append(cmds, append([]string{"ydotool", "key"}, ydotoolKeys[keys]...),
				append([]string{"ydotool", "key"}, ydotoolKeys["enter"]...))
		} else if len(cmds) == 0 { // ponytail: with wtype installed, an old ydotool is skipped silently
			return errors.New("ydotool 0.1.x is not supported: install ydotool 1.x")
		}
	}
	if len(cmds) == 0 {
		return errors.New("no paste tool: install wtype or ydotool 1.x")
	}
	var failed []string
	for i := 0; i < len(cmds); i += 2 { // the paste chord, then the same tool's Enter
		cmd := cmds[i]
		if run(cmd[0], cmd[1:], nil) == nil {
			slog.Info("paste", "tool", cmd[0], "ms", time.Since(start).Milliseconds())
			if !enter {
				return nil
			}
			time.Sleep(enterDelay)
			if err := run(cmd[0], cmds[i+1][1:], nil); err != nil {
				return fmt.Errorf("pasted, but %s could not press Enter: %w", cmd[0], err)
			}
			return nil
		}
		msg := cmd[0] + " failed"
		if cmd[0] == "ydotool" {
			msg += ": is ydotoold running?"
		}
		failed = append(failed, msg)
	}
	return errors.New(strings.Join(failed, "; "))
}

// run runs a tool with stdin and a 5 s limit; its output goes to /dev/null.
func run(name string, args []string, stdin *strings.Reader) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	if stdin != nil {
		cmd.Stdin = stdin
	}
	cmd.WaitDelay = time.Second
	if err := cmd.Run(); !errors.Is(err, exec.ErrWaitDelay) {
		return err
	}
	return nil
}

// Notify prints to stderr and, when desktop is true, shows a desktop notification over D-Bus (best effort: no bus
// or no notification daemon means stderr only). ponytail: a var so tests can record the notifications.
var Notify = func(summary, body string, desktop bool) {
	if body != "" {
		fmt.Fprintf(os.Stderr, "%s\n%s\n", summary, body)
	} else {
		fmt.Fprintln(os.Stderr, summary)
	}
	if desktop {
		show(summary, body, map[string]dbus.Variant{})
	}
}

// ConfigError reports an invalid config.toml: "Invalid config: <err>" on stderr and, when desktop is true, a
// notification titled Invalid config with the error as its body.
func ConfigError(err error, desktop bool) {
	slog.Error("invalid config", "err", err)
	fmt.Fprintln(os.Stderr, "Invalid config:", err)
	if desktop {
		show("Invalid config", err.Error(), map[string]dbus.Variant{})
	}
}

// Terminal reports whether f is a terminal.
func Terminal(f *os.File) bool {
	_, err := unix.IoctlGetTermios(int(f.Fd()), unix.TCGETS)
	return err == nil
}

// Critical shows a notification that stays up until CloseNotification(id) (GNOME keeps critical ones on screen) and
// returns its id; 0 when none shows. Transient: not kept in the message list.
func Critical(summary, body string) uint32 {
	return show(summary, body, map[string]dbus.Variant{"urgency": dbus.MakeVariant(byte(2)),
		"transient": dbus.MakeVariant(true)})
}

// CloseNotification closes the notification id (0: none).
func CloseNotification(id uint32) {
	if id != 0 {
		notifications("CloseNotification", id)
	}
}

func show(summary, body string, hints map[string]dbus.Variant) uint32 {
	var id uint32
	notifications("Notify", "Habla", uint32(0), "audio-input-microphone", summary, body, []string{}, hints, int32(-1)).
		Store(&id)
	return id
}

// notifications calls a method of the notification daemon; the call carries the error (no bus, no daemon).
func notifications(method string, args ...any) *dbus.Call {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return &dbus.Call{Err: err}
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second) // D-Bus activation can hang without a daemon
	defer cancel()
	return conn.Object("org.freedesktop.Notifications", "/org/freedesktop/Notifications").CallWithContext(ctx,
		"org.freedesktop.Notifications."+method, 0, args...)
}
