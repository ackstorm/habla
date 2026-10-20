# internal/tray

## Menu swap (`refresh`)

- Ayatana's `set_menu` drops its ref on the old menu, which then finalizes (with a session bus): destroy the old menu
  before `set_menu`, never after (that was `gtk_widget_destroy` on freed memory).
- Ayatana checks the middle-click target against the menu it holds, in `set_secondary_activate_target` and again in
  `set_menu`: a target in the other menu walks up to an unattached GtkMenu and calls `gtk_widget_get_parent(NULL)`.
  So the target is cleared (0) before the swap and set after it.
- `startTray` fails any GUI test whose tray output has a `-CRITICAL **` line.
- `startTray`'s `stop` waits for the tray's whole process group, not just `cmd`: `dbus-run-session` (private bus)
  exits on SIGTERM without waiting for `habla tray` and its `dbus-daemon`, so a still-running tray wrote into the
  test's directories while `t.TempDir` removed them (`RemoveAll … directory not empty`, flaky on CI only).

## GlobalShortcuts portal (`portal.go`)

- `Register` (Registry) must be the first portal call on its connection, hence `dbus.ConnectSessionBus()` of its own;
  its error is ignored (portals before 1.19 have no Registry). `AppID` and `shortcutID` ("dictate") never change:
  the desktop stores the confirmed key under them and shows its dialog again for new ones.
- Request/Response: the Response match (path `/org/freedesktop/portal/desktop/request/SENDER/TOKEN`, SENDER =
  unique name without `:` and `.` → `_`) is added before the call; the fake may emit Response before the reply.
- godbus `Call`/`GetProperty` wait forever (context.Background); GDBus's -1 meant 25 s. Every portal method call goes
  through `call` (`callTimeout`, 25 s; a var for tests); only the Response wait (the user's dialog) is unbounded.
- godbus's `Signal` channel blocks its reader when full, then delivers from extra goroutines (out of order): keep it
  buffered and drained. `request` drops unrelated signals while it waits; keys can't come before the bind answer.
- The real dialog and keys are checked by hand on KDE/GNOME Wayland; the dev box's portal has no GlobalShortcuts
  backend. `portal_test.go` runs `testenv`'s `fake-portal` under `dbus-run-session`, headless, in the default
  `go test` (skipped only without `dbus-run-session`). Never test "no portal on the bus" with a real
  `dbus-run-session`: its session config would D-Bus-activate the real `xdg-desktop-portal`.

## Recording pill (`pill.go`)

- Paper light: 575 × 46 px white body plus 4 px shadow space per side, pale border, rose recording dot,
  28 lavender peak-history bars across 115 px (last 240 ms) at ~30 fps. Peak scale is -50..0 dBFS; amber starts
  at -20 dBFS and red at -12 dBFS, indicating speech intensity rather than clipping. Immediate attack / 140 ms decay
  affects the display only, never recorded audio.
  The idle waveform is a continuous 1 px line. Display input at or below -40 dBFS stays flat; speech bars use
  `round(1 + sqrt(max(0, (level - 0.2) / 0.8)) * 23)` up to 24 px (-30 dBFS speech fills about half the height);
  colours still reflect actual peaks. The icon/meter section takes 30% of the width; text/timer/ESC take 70%.
  Text, icons and controls are vertically anchored to the bar's centre. Both processing sweeps take 650 ms each way.
- `show(phase, hint)` preserves the window, width, position and last recording timer. `busy`/`transform` add a
  phase timer after the title (`PhaseLabel`, "Transcribing (0.8s)"): it starts at `state`'s mtime (`StateWritten`) when `show`
  enters the phase and freezes when a frame sees a newer write (copying/pasting); `frame` reserves its width. An aqua spinner/sweep identifies
  `busy` (transcribing), a gold shimmering star/sweep identifies `transform`. Delivery retains the last visible phase
  without separate labels;
  Esc disappears once copying starts. `copy_failed` stays red until the next dictation. The Wayland notification
  fallback follows the visible phases, closing its previous notification before replacing it, without delivery banners.
- Drawn entirely with cairo in one DrawingArea (no labels bound): `pillText` measures and paints the phase text
  and right-aligned recording timer for both sizing
  (on `measure`, a 1x1 image-surface context, so the size is known before the first draw) and painting. Each frame
  refits the window: `gtk_window_resize`, since a GTK window grows by itself but never shrinks.
- `cairo_show_text` leaves a current point, and `cairo_arc` draws a line from it: `roundRect`/`circle` `MoveTo` the
  arc's start first.
- xgb gives device pixels, GDK app pixels (GDK_SCALE=2): `place` divides by the scale before `PlacePill`.
- `held` comes from the last hotkey/portal action (`act` sets it on the GTK thread before `listen`), so it is set
  before the state file changes; menu and CLI recordings keep `false` → "Stop or discard from the tray".
- GUI tests: Xvfb has one RandR output, and GDK lists only monitors with a connected output, so `xrandr
  --setmonitor` monitors are invisible to it (xrandr isn't installed anyway): `TestPlacePill` covers two monitors.
  GDK also ignores `_NET_WORKAREA` without a WM that lists it in `_NET_SUPPORTED` (and for non-primary monitors).
  No WM either, so tests set `_NET_ACTIVE_WINDOW` on the root themselves (`xdotool windowactivate` needs a WM).
  Xvfb is not composited: the window has a white background outside the rounded border.
- The notice test runs GTK on Xvfb with `WAYLAND_DISPLAY` set: `desktop.Wayland()` is true and
  `gtk_layer_is_supported` is false (an X11 GdkDisplay), the GNOME path. `fake-portal` doubles as the notification
  daemon (`notifications` lines), on the bus `fakeBus` starts; `startTray(t, e, false)` uses that bus.
