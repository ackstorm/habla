package tray

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/ackstorm/habla/internal/config"
	"github.com/ackstorm/habla/internal/desktop"
	"github.com/ackstorm/habla/internal/gtk"
	"github.com/ackstorm/habla/internal/hotkey"
	"github.com/ackstorm/habla/internal/logs"
	"github.com/ackstorm/habla/internal/rundir"
	"github.com/godbus/dbus/v5"
)

// GTK runs on the main OS thread: main() starts there, and this keeps the main goroutine on it.
func init() { runtime.LockOSThread() }

var descriptions = map[string]string{"idle": "Habla", "recording": "Habla: recording", "busy": "Habla: transcribing",
	"transform": "Habla: transforming",
	"failed":    "Habla: transcription failed", "copying": "Habla: copying to clipboard", "pasting": "Habla: pasting",
	"copy_failed": "Habla: copy failed"}

const hostWait = 5 * time.Second // at login the panel may start after Habla

// Main is `habla tray`.
func Main(args []string) int {
	fs := flag.NewFlagSet("habla tray", flag.ContinueOnError)
	install := fs.Bool("install-autostart", false, "start the tray at login, and install its icons and desktop entry")
	logLevel := fs.String("log", "", "log at `LEVEL` (error, warn, info, debug) for this run and its listen runs")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if err := logs.Setup("tray", *logLevel); err != nil {
		fmt.Fprintln(os.Stderr, "habla tray:", err)
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintln(os.Stderr, "usage: habla tray [--install-autostart]")
		return 2
	}
	if *install {
		return installAutostart()
	}
	cfg, err := config.Load()
	if err != nil {
		desktop.ConfigError(err, true)
		return 1
	}
	logs.SetLevel(cfg.LogLevel)
	lock, err := AcquireLock()
	if err != nil {
		fmt.Fprintln(os.Stderr, "habla tray:", err)
		return 1
	}
	if lock == nil {
		fmt.Fprintln(os.Stderr, "Habla tray is already running")
		return 1
	}
	defer lock.Close()
	if err := gtk.Load(); err != nil {
		fmt.Fprintln(os.Stderr, "habla tray:", err)
		return 1
	}
	if err := gtk.Init(); err != nil {
		fmt.Fprintln(os.Stderr, "habla tray:", err)
		return 1
	}
	t, err := newTray(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "habla tray:", err)
		return 1
	}
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	go func() { <-quit; gtk.Idle(gtk.MainQuit) }()
	gtk.Main()
	if t.hotkey != nil {
		t.hotkey.close() // a recording in progress is a detached process and keeps going
	}
	return 0
}

// installAutostart writes the autostart entry, the icons into ~/.local/share/icons and the portal's desktop entry.
func installAutostart() int {
	icons, err := WriteIcons(filepath.Join(dataDir(), "icons"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "habla tray:", err)
		return 1
	}
	fmt.Println("Wrote " + icons)
	for _, install := range []func() (string, error){InstallAutostart, InstallDesktopEntry} {
		path, err := install()
		if err != nil {
			fmt.Fprintln(os.Stderr, "habla tray:", err)
			return 1
		}
		fmt.Println("Wrote " + path)
	}
	return 0
}

// shown is what the menu shows; it is rebuilt only when this changes.
type shown struct {
	recording, failed bool
	choices           map[string][]Choice
	paste, pasteEnter bool
}

type tray struct {
	indicator, menu uintptr
	queue           [][]string // `habla listen` commands, run one at a time
	timer           uint32     // the label's 1 s timeout while recording; 0 when none
	shown           *shown
	cfg             *config.Config // the last valid config: the menu keeps it while the file is invalid
	startLabel      string
	key             string // the X11 hotkey's label; "" for the portal (the user picks the key) or none
	held            bool   // the last hold-to-talk action was --start: the recording ends on release
	hotkey          *x11Hotkey
	busyGrabbed     bool          // Esc and Enter are grabbed during transcription or transform
	pill            recordingView // nil: the tray icon alone
	pillState       string
	pillPhase       string // the phase and hint the pill shows
	pillHint        []Segment
	pending         atomic.Bool // a refresh is queued on the GTK thread
}

func newTray(cfg *config.Config) (*tray, error) {
	icons := filepath.Join(rundir.Dir(), "icons")
	if _, err := WriteIcons(icons); err != nil {
		return nil, err
	}
	t := &tray{startLabel: "Start dictation", cfg: cfg}
	t.indicator = gtk.IndicatorNew("habla", "habla-idle-symbolic", gtk.CategoryApplicationStatus)
	gtk.IndicatorSetIconThemePath(t.indicator, icons)
	gtk.IndicatorSetStatus(t.indicator, gtk.StatusActive)
	t.grab(cfg.Tray.Hotkey)
	hotkeyMode := "x11"
	if PortalDesktop() {
		hotkeyMode = "portal"
	} else if desktop.Wayland() {
		hotkeyMode = "none"
	}
	slog.Info("tray started", "hotkey", hotkeyMode)
	t.pill = newPill() // before the first refresh, which may show it
	if err := watch([]string{rundir.Dir(), filepath.Dir(config.Path())}, t.queueRefresh); err != nil {
		fmt.Fprintln(os.Stderr, "habla tray: the icon will not follow changes:", err) // the menu still works
	}
	t.refresh()
	time.AfterFunc(hostWait, checkHost)
	return t, nil
}

// grab sets up hold-to-talk: the X11 hotkey, the portal shortcut on KDE/GNOME Wayland, or nothing on sway and
// Hyprland (one notification says why when it can't).
func (t *tray) grab(text string) {
	act := func(flag string) { gtk.Idle(func() { t.held = flag == "--start"; t.listen(flag) }) }
	if PortalDesktop() {
		trigger, err := hotkey.PortalTrigger(text)
		if err != nil {
			notify(fmt.Sprintf("Invalid hotkey %s: %v", text, err),
				fmt.Sprintf("Change the line hotkey = %q in %s.", text, config.Path()))
			return
		}
		go runPortal(trigger, act) // the portal's key is whatever the user confirms, so the menu names none
		return
	}
	if desktop.Wayland() {
		return // sway and Hyprland bind the keys in their config (README)
	}
	line := fmt.Sprintf("Change the line hotkey = %q in %s and restart habla tray.", text, config.Path())
	h, err := grabX11(text, act)
	var invalid invalidHotkey
	var display noDisplay
	switch {
	case err == nil:
		t.hotkey = h
		if key, err := hotkey.Label(text); err == nil {
			t.key = key
			t.startLabel += " (hold " + key + ")"
		}
	case errors.Is(err, errTaken):
		notify(text+" is taken by another app", line)
	case errors.As(err, &invalid):
		notify(fmt.Sprintf("Invalid hotkey %s: %v", text, err), line)
	case errors.As(err, &display):
		notify("Hold-to-talk is off: no X display", err.Error())
	}
}

// queueRefresh runs refresh once on the GTK thread for any number of calls before it runs. Safe from any goroutine.
func (t *tray) queueRefresh() {
	if t.pending.CompareAndSwap(false, true) {
		gtk.Idle(func() { t.pending.Store(false); t.refresh() })
	}
}

// refresh redraws the icon, the timer, the pill and (when what it shows changed) the menu from the runtime files and
// config. Every way to record (the hotkey, the portal, the menu, habla listen) writes state, so all show the pill.
func (t *tray) refresh() {
	state := rundir.ReadState()
	icon := state
	if state == "transform" || state == "copying" || state == "pasting" {
		icon = "busy"
	} else if state == "copy_failed" {
		icon = "failed"
	}
	gtk.IndicatorSetIconFull(t.indicator, "habla-"+icon+"-symbolic", descriptions[state])
	if busy := rundir.Processing(state); t.hotkey != nil && busy != t.busyGrabbed {
		t.hotkey.grabBusyKeys(busy)
		t.busyGrabbed = busy
	}
	if t.hotkey != nil && !rundir.Processing(state) && state != "copying" {
		t.hotkey.unlockFocus(true) // the focus lock after Enter lasts until the paste
	}
	if state == "recording" && t.timer == 0 {
		t.timer = gtk.Timeout(1000, t.tick)
		t.setLabel()
	} else if state != "recording" {
		if id := t.timer; id != 0 {
			t.timer = 0
			gtk.SourceRemove(id)
		}
		gtk.IndicatorSetLabel(t.indicator, "", "")
	}
	if t.pillState != state {
		t.pillState = state
		phase, key := state, t.key
		if state == "copying" || state == "pasting" {
			phase, key = "busy", "" // no separate delivery display, and cancellation is no longer available
			if rundir.AfterTransform() {
				phase = "transform"
			}
		}
		hint := PillHint(phase, key, t.held)
		if t.pill != nil {
			if state == "idle" || state == "failed" {
				t.pill.hide()
			} else if phase != t.pillPhase || !slices.Equal(hint, t.pillHint) {
				t.pill.show(phase, hint)
			}
		}
		t.pillPhase, t.pillHint = phase, hint
		if rundir.Processing(state) || state == "copying" || state == "pasting" {
			// A process that dies leaves no file event; this also clears the pill on Wayland.
			gtk.Timeout(1000, func() bool {
				if rundir.ReadState() == state {
					return true
				}
				t.refresh()
				return false
			})
		}
	}
	if cfg, err := config.Load(); err == nil { // invalid: the next habla listen reports it
		t.cfg = cfg
	}
	_, err := os.Stat(filepath.Join(rundir.Dir(), "failed.wav"))
	now := &shown{state == "recording", err == nil, MenuChoices(t.cfg), t.cfg.Tray.Paste, t.cfg.Tray.PasteEnter}
	if !reflect.DeepEqual(now, t.shown) {
		t.shown = now
		// Ayatana checks the middle-click target against the menu it holds, in set_menu too: a target in the other
		// menu walks up to an unattached GtkMenu and into gtk_widget_get_parent(NULL). No target across the swap.
		gtk.IndicatorSetSecondaryActivateTarget(t.indicator, 0)
		if t.menu != 0 {
			gtk.WidgetDestroy(t.menu) // while the indicator still holds it: set_menu drops the last ref
		}
		var toggle uintptr
		t.menu, toggle = t.buildMenu(now)
		gtk.IndicatorSetMenu(t.indicator, t.menu)
		gtk.IndicatorSetSecondaryActivateTarget(t.indicator, toggle) // middle-click
	}
}

func (t *tray) setLabel() {
	gtk.IndicatorSetLabel(t.indicator, ElapsedLabel(RecordingSeconds()), "0:00")
}

// tick updates the elapsed-time label once a second while recording.
func (t *tray) tick() bool {
	if rundir.ReadState() != "recording" { // ended without a file event (the recording process died)
		t.timer = 0 // the source ends with this return: its id is stale
		t.refresh()
		return false
	}
	t.setLabel()
	return true
}

// buildMenu returns the menu and its Start/Stop item (the middle-click target).
func (t *tray) buildMenu(s *shown) (menu, toggle uintptr) {
	menu = gtk.MenuNew()
	label := t.startLabel
	if s.recording {
		label = "Stop dictation"
	}
	toggle = t.command(menu, label, "--toggle")
	if s.recording {
		t.command(menu, "Discard dictation", "--cancel") // Wayland has no Esc
	}
	if s.failed {
		t.command(menu, "Retry last failed", "--retry")
	}
	gtk.MenuShellAppend(menu, gtk.SeparatorMenuItemNew())
	for _, group := range [][]struct{ key, title string }{ // quick choices, then the active providers
		{{"transform", "Transform"}},
		{{"stt_provider", "STT provider"}, {"llm_provider", "LLM provider"}},
	} {
		appended := false
		for _, r := range group {
			if len(s.choices[r.key]) > 0 {
				gtk.MenuShellAppend(menu, t.radio(r.key, r.title, s.choices[r.key]))
				appended = true
			}
		}
		if appended {
			gtk.MenuShellAppend(menu, gtk.SeparatorMenuItemNew())
		}
	}
	paste := gtk.CheckMenuItemNewWithLabel("Also paste automatically")
	gtk.CheckMenuItemSetActive(paste, s.paste)
	gtk.Connect(paste, "toggled", func() { t.choose("paste", gtk.CheckMenuItemGetActive(paste)) })
	gtk.MenuShellAppend(menu, paste)
	enter := gtk.CheckMenuItemNewWithLabel("And press Enter")
	gtk.CheckMenuItemSetActive(enter, s.pasteEnter)
	gtk.WidgetSetSensitive(enter, s.paste) // Enter follows the paste
	gtk.Connect(enter, "toggled", func() { t.choose("paste_enter", gtk.CheckMenuItemGetActive(enter)) })
	gtk.MenuShellAppend(menu, enter)
	gtk.MenuShellAppend(menu, gtk.SeparatorMenuItemNew())
	edit := gtk.MenuItemNewWithLabel("Edit config…")
	gtk.Connect(edit, "activate", func() { start(exec.Command("xdg-open", config.Path())) })
	gtk.MenuShellAppend(menu, edit)
	quit := gtk.MenuItemNewWithLabel("Quit") // a recording in progress is a detached process and keeps going
	gtk.Connect(quit, "activate", gtk.MainQuit)
	gtk.MenuShellAppend(menu, quit)
	gtk.WidgetShowAll(menu)
	return menu, toggle
}

func (t *tray) command(menu uintptr, label, flag string) uintptr {
	item := gtk.MenuItemNewWithLabel(label)
	gtk.Connect(item, "activate", func() { t.listen(flag) })
	gtk.MenuShellAppend(menu, item)
	return item
}

func (t *tray) radio(key, title string, choices []Choice) uintptr {
	submenu, group, current := gtk.MenuNew(), uintptr(0), title
	for _, c := range choices {
		item := gtk.RadioMenuItemNewWithLabelFromWidget(group, c.Label)
		group = item
		gtk.CheckMenuItemSetActive(item, c.Active)
		// Connected after SetActive, so building the menu writes nothing; "toggled" fires for both ends.
		value := c.Value
		gtk.Connect(item, "toggled", func() {
			if gtk.CheckMenuItemGetActive(item) {
				t.choose(key, value)
			}
		})
		gtk.MenuShellAppend(submenu, item)
		if c.Active {
			current = title + ": " + c.Label
		}
	}
	parent := gtk.MenuItemNewWithLabel(current)
	gtk.MenuItemSetSubmenu(parent, submenu)
	return parent
}

func (t *tray) choose(key string, value any) {
	if err := config.SaveChoice(key, value); err != nil {
		fmt.Fprintln(os.Stderr, "habla tray:", err)
	}
	gtk.Idle(t.refresh) // rebuild once the menu has closed
}

// listen runs `habla listen <flag>`. One at a time: a --stop must not overtake the --start still starting.
func (t *tray) listen(flag string) {
	var opts []string // [tray] paste settings; habla listen reads none of them itself
	switch {
	case t.cfg.Tray.Paste && t.cfg.Tray.PasteEnter:
		opts = []string{"--paste_enter"}
	case t.cfg.Tray.Paste:
		opts = []string{"--paste"}
	}
	if EnqueueListen(&t.queue, flag, opts...) {
		t.spawn()
	}
}

func (t *tray) spawn() {
	cmd := exec.Command(t.queue[0][0], t.queue[0][1:]...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	flag, start := cmd.Args[len(cmd.Args)-1], time.Now()
	if err := cmd.Start(); err != nil {
		t.queue = nil
		notify("Could not run habla listen", err.Error())
		return
	}
	go func() {
		cmd.Wait()
		slog.Info("listen exited", "flag", flag, "code", cmd.ProcessState.ExitCode(), "ms", time.Since(start).Milliseconds())
		gtk.Idle(t.spawned)
	}()
}

func (t *tray) spawned() {
	t.queue = t.queue[1:]
	if len(t.queue) > 0 {
		t.spawn()
	}
}

// start runs a helper (xdg-open) and reaps it.
func start(cmd *exec.Cmd) {
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "habla tray:", err)
		return
	}
	go cmd.Wait()
}

// notify shows a notification without blocking the GTK thread on D-Bus.
func notify(summary, body string) { go desktop.Notify(summary, body, true) }

// checkHost notifies once when nothing shows the icon: no StatusNotifierItem host and no XEmbed tray (GNOME without
// the extension). The hotkey keeps working either way.
func checkHost() {
	if !watcherHasHost() && !xembedTray() {
		desktop.Notify("No tray to show Habla's icon", "On GNOME, install the AppIndicator and KStatusNotifierItem "+
			"Support extension. The hotkey and habla listen keep working.", true)
	}
}

func watcherHasHost() bool {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return false
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var registered dbus.Variant
	err = conn.Object("org.kde.StatusNotifierWatcher", "/StatusNotifierWatcher").CallWithContext(ctx,
		"org.freedesktop.DBus.Properties.Get", dbus.FlagNoAutoStart, "org.kde.StatusNotifierWatcher",
		"IsStatusNotifierHostRegistered").Store(&registered)
	ok, _ := registered.Value().(bool)
	return err == nil && ok
}
