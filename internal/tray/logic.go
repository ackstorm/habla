// Package tray is `habla tray`. logic.go is the pure part, tested without a display;
// the GTK side lives in other files of this package.
package tray

import (
	"cmp"
	"embed"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/ackstorm/habla/internal/config"
	"github.com/ackstorm/habla/internal/desktop"
	"github.com/ackstorm/habla/internal/rundir"
	"golang.org/x/sys/unix"
)

// Icons holds habla-{idle,recording,busy,failed}-symbolic.svg under icons/.
//
//go:embed icons/*.svg
var Icons embed.FS

// AppID is the GlobalShortcuts portal's app ID: the basename of the .desktop file below. Never change it.
const AppID = "io.github.ackstorm.Habla"

const autostart = `[Desktop Entry]
Type=Application
Name=Habla
Comment=Push-to-dictate: tray icon and hold-to-talk hotkey
Exec=%s tray
Icon=audio-input-microphone
Terminal=false
X-GNOME-Autostart-enabled=true
`

// Segment is a piece of the pill's hint; Key marks a keycap.
type Segment struct {
	Text string
	Key  bool
}

// Choice is one radio item of the menu.
type Choice struct {
	Label, Value string
	Active       bool
}

var readState = rundir.ReadState // tests replace it: "busy" needs a live listen process

// HablaPath is the absolute path of the running binary (symlinks resolved). The session's PATH may lack
// ~/.local/bin, and rundir.Alive needs an argv entry named habla.
func HablaPath() string {
	exe, err := os.Executable()
	if err != nil {
		return "habla"
	}
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	return exe
}

func writeEntry(path string) (string, error) {
	// ponytail: the path is not quoted for Desktop Entry Exec rules; install paths have no spaces.
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	return path, os.WriteFile(path, []byte(fmt.Sprintf(autostart, HablaPath())), 0o644)
}

// InstallAutostart writes $XDG_CONFIG_HOME/autostart/habla.desktop (XDG Autostart), replacing an older one.
func InstallAutostart() (string, error) {
	return writeEntry(filepath.Join(filepath.Dir(filepath.Dir(config.Path())), "autostart", "habla.desktop"))
}

// InstallDesktopEntry writes $XDG_DATA_HOME/applications/io.github.ackstorm.Habla.desktop. An unsandboxed app gets a portal
// app ID only by naming a .desktop file whose Exec the portal can find, hence the absolute path. Not NoDisplay:
// GNOME Settings lists an app's global shortcuts under Apps, which skips hidden entries.
func InstallDesktopEntry() (string, error) {
	return writeEntry(filepath.Join(dataDir(), "applications", AppID+".desktop"))
}

// dataDir is $XDG_DATA_HOME, by default ~/.local/share.
func dataDir() string {
	if data := os.Getenv("XDG_DATA_HOME"); data != "" {
		return data
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share")
}

// WriteIcons writes the embedded icons to base/hicolor/scalable/status/ (an icon theme directory) and returns that
// directory.
func WriteIcons(base string) (string, error) {
	dir := filepath.Join(base, "hicolor", "scalable", "status")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	entries, _ := Icons.ReadDir("icons")
	for _, e := range entries {
		data, _ := Icons.ReadFile("icons/" + e.Name())
		if err := os.WriteFile(filepath.Join(dir, e.Name()), data, 0o644); err != nil {
			return "", err
		}
	}
	return dir, nil
}

// PortalDesktop is true on KDE or GNOME under Wayland: hold-to-talk goes through the GlobalShortcuts portal. sway
// and Hyprland bind the keys in their own config.
func PortalDesktop() bool {
	names := strings.Split(strings.ToUpper(os.Getenv("XDG_CURRENT_DESKTOP")), ":")
	return desktop.Wayland() && (slices.Contains(names, "KDE") || slices.Contains(names, "GNOME"))
}

// AcquireLock takes an exclusive flock on tray.lock for the life of the process. It returns nil, nil when another
// tray holds it. os.OpenFile sets O_CLOEXEC, so a detached `habla listen` never keeps the lock after Quit.
func AcquireLock() (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(rundir.Dir(), "tray.lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, nil
		}
		return nil, err
	}
	return f, nil
}

// ElapsedLabel is the recording timer: 7 → 0:07.
func ElapsedLabel(seconds float64) string {
	s := max(0, int(seconds))
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}

// RecordingSeconds is the time since `state` was written: while recording, since the recording started. 0 without it.
func RecordingSeconds() float64 {
	info, err := os.Stat(filepath.Join(rundir.Dir(), "state"))
	if err != nil {
		return 0
	}
	return max(0, time.Since(info.ModTime()).Seconds())
}

// The pill's peaks: -50 dBFS or quieter = empty, 0 dBFS = full.
const levelFloorDB, levelRangeDB = -50.0, 50.0

// Levels is the pill's sound-level bars: the last `seconds` of a recording in progress (16 kHz mono s16), split into
// `bars` time slices, each peak 0.0 to 1.0, oldest to newest. The recorder is still writing, so sizes mean nothing: read the
// raw tail of the file.
func Levels(wav string, bars int, seconds float64) []float64 {
	result := make([]float64, bars)
	f, err := os.Open(wav)
	if err != nil {
		return result
	}
	defer f.Close()
	size, err := f.Seek(0, 2)
	if err != nil {
		return result
	}
	end := size - size%2 // an odd size: a sample half written. ponytail: assumes an even-sized header.
	start := max(44, end-2*int64(16000*seconds))
	if end <= start {
		return result
	}
	data := make([]byte, end-start)
	if _, err := f.ReadAt(data, start); err != nil {
		return result
	}
	samples := make([]int16, len(data)/2) // ponytail: little-endian, as the WAV is
	for i := range samples {
		samples[i] = int16(uint16(data[2*i]) | uint16(data[2*i+1])<<8)
	}
	step := max(1, int(16000*seconds)/bars)
	for i := range result {
		chunk := samples[max(0, len(samples)-(bars-i)*step):max(0, len(samples)-(bars-i-1)*step)]
		var peak float64
		for _, s := range chunk {
			peak = max(peak, math.Abs(float64(s)))
		}
		if peak > 0 {
			result[i] = min(1, max(0, (20*math.Log10(peak/32768)-levelFloorDB)/levelRangeDB))
		}
	}
	return result
}

// PillHint is the stop/discard hint: a held X11 key has a label and Esc; a held portal shortcut has neither.
// Without a held shortcut, the tray menu has Stop and Discard.
func PillHint(phase, key string, held bool) []Segment {
	if phase != "recording" {
		label := map[string]string{"busy": "Transcribing…", "transform": "Transforming…",
			"copy_failed": "Copy failed · see notification"}[phase]
		hint := []Segment{{label, false}}
		if rundir.Processing(phase) && key != "" {
			hint = append(hint, Segment{" · ", false}, Segment{"Esc", true}, Segment{" discard", false})
		}
		return hint
	}
	switch {
	case !held:
		return []Segment{{"Stop or discard from the tray", false}}
	case key == "":
		return []Segment{{"Release the shortcut to stop · discard from the tray", false}}
	}
	return []Segment{{"Release ", false}, {key, true}, {" to stop · ", false}, {"Esc", true}, {" discard", false}}
}

// MenuChoices are the menu's radio lists, keyed by their config.SaveChoice name. The transform presets come after
// "none" in file order. A configured model, voice or language missing from its list is appended, active. A service
// with two or more providers gets a <service>_provider list; tts_voice and live_voice list the active provider's voices.
func MenuChoices(c *config.Config) map[string][]Choice {
	choices := map[string][]Choice{}
	if c.STT != nil {
		choices["stt_model"] = modelChoices(&c.STT.Conn)
		languages := []Choice{{"Auto", "", c.STT.Language == ""}}
		for _, l := range c.STT.Languages {
			languages = append(languages, Choice{l, l, l == c.STT.Language})
		}
		if !slices.ContainsFunc(languages, func(ch Choice) bool { return ch.Active }) {
			languages = append(languages, Choice{c.STT.Language, c.STT.Language, true})
		}
		choices["language"] = languages
		if c.LLM != nil {
			transform := []Choice{{"None: raw transcript", "", c.STT.Transform == ""}}
			for _, name := range c.TransformOrder {
				transform = append(transform, Choice{presetLabel(name), name, name == c.STT.Transform})
			}
			choices["transform"] = transform
		}
	}
	if c.LLM != nil {
		choices["llm_model"] = modelChoices(c.LLM)
	}
	if c.TTS != nil {
		choices["tts_model"] = modelChoices(&c.TTS.Conn)
	}
	for _, service := range []string{"stt", "llm", "tts", "live"} {
		p := c.Providers[service]
		if len(p.Names) < 2 {
			continue
		}
		list := []Choice{}
		for _, name := range p.Names {
			list = append(list, Choice{name, name, name == p.Active})
		}
		choices[service+"_provider"] = list
	}
	if c.TTS != nil {
		choices["tts_voice"] = voiceChoices(c.TTS.Voices, c.TTS.Voice)
	}
	if c.Live != nil && (c.Live.Voice != "" || len(c.Live.Voices) > 0) {
		choices["live_voice"] = voiceChoices(c.Live.Voices, c.Live.Voice)
	}
	return choices
}

// modelChoices lists a service's models, with its configured model appended, active, when the list lacks it.
func modelChoices(conn *config.Conn) []Choice { return options(conn.Models, conn.Model) }

// options lists choices, with current appended, active, when the list lacks it ("" is never appended).
func options(list []string, current string) []Choice {
	out := []Choice{}
	for _, v := range list {
		out = append(out, Choice{v, v, v == current})
	}
	if current != "" && !slices.Contains(list, current) {
		out = append(out, Choice{current, current, true})
	}
	return out
}

// voiceChoices lists "Name=ID" entries by name (the ID when unnamed); the name, or the ID, is what is saved as voice.
// An entry is active when its name or ID is current. A current voice in no entry is appended, active.
func voiceChoices(voices []string, current string) []Choice {
	out, found := []Choice{}, false
	for _, e := range voices {
		name, id := config.VoiceEntry(e)
		name = cmp.Or(name, id)
		active := current == name || current == id
		found = found || active
		out = append(out, Choice{name, name, active})
	}
	if current != "" && !found {
		out = append(out, Choice{current, current, true})
	}
	return out
}

// presetLabel is Python's str.capitalize: first rune upper, the rest lower; "" stays "".
func presetLabel(name string) string {
	if name == "" {
		return ""
	}
	r, n := utf8.DecodeRuneInString(name)
	return string(unicode.ToUpper(r)) + strings.ToLower(name[n:])
}

// EnqueueListen queues `habla listen [opts] flag` and reports whether the caller should spawn the first child.
func EnqueueListen(queue *[][]string, flag string, opts ...string) bool {
	state := readState()
	q := *queue
	if len(q) > 0 && q[0][len(q[0])-1] == "--retry" {
		return false // ignore input now, rather than recording after a synchronous retry exits
	}
	if flag == "--retry" && (len(q) > 0 || state == "recording" || rundir.Processing(state)) {
		return false // a retry must not block a pending stop/cancel during startup, nor join a busy dictation
	}
	*queue = append(q, append(append([]string{HablaPath(), "listen"}, opts...), flag))
	return len(*queue) == 1
}

// Rect, Point and Size are in GDK application pixels.
type (
	Rect  struct{ X, Y, Width, Height int }
	Point struct{ X, Y int }
	Size  struct{ Width, Height int }
)

const pillMargin = 48 // px between the pill and the bottom of the work area

// PlacePill is where the X11 pill goes: centred, pillMargin above the bottom of the work area of the monitor that
// holds the active window's centre (without one, the pointer). A point outside every monitor takes the nearest one,
// as gdk_display_get_monitor_at_point does. No monitors (a closed lid, mid-hotplug): 0, 0.
func PlacePill(monitors, workareas []Rect, active *Rect, pointer Point, size Size) (x, y int) {
	if len(monitors) == 0 {
		return 0, 0
	}
	at := pointer
	if active != nil {
		at = Point{active.X + active.Width/2, active.Y + active.Height/2}
	}
	best, nearest := 0, -1
	for i, m := range monitors {
		dx := max(m.X-at.X, 0, at.X-(m.X+m.Width-1))
		dy := max(m.Y-at.Y, 0, at.Y-(m.Y+m.Height-1))
		if d := dx*dx + dy*dy; nearest < 0 || d < nearest {
			best, nearest = i, d
		}
	}
	w := workareas[best]
	return w.X + (w.Width-size.Width)/2, w.Y + w.Height - size.Height - pillMargin
}
