package tray

import (
	"encoding/binary"
	"encoding/xml"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ackstorm/habla/internal/config"
	"github.com/ackstorm/habla/internal/rundir"
	"github.com/ackstorm/habla/internal/testenv"
)

func TestMain(m *testing.M) {
	testenv.Fake()
	os.Exit(run(m)) // tray_gui_test.go: builds habla first under HABLA_GUI=1
}

func TestElapsedLabel(t *testing.T) {
	for seconds, shown := range map[float64]string{0: "0:00", 7.9: "0:07", 65: "1:05", 600: "10:00", -3: "0:00"} {
		if got := ElapsedLabel(seconds); got != shown {
			t.Errorf("ElapsedLabel(%v) = %q, want %q", seconds, got, shown)
		}
	}
}

func loaded(t *testing.T) *config.Config {
	t.Helper()
	c, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestMenuChoicesFromTheDefaultConfig(t *testing.T) {
	testenv.Setup(t)
	got := MenuChoices(loaded(t))
	want := map[string][]Choice{
		"stt_provider": {{"openai", "openai", true}, {"gemini", "gemini", false}},
		"llm_provider": {{"openai", "openai", true}, {"gemini", "gemini", false}},
		"transform": {
			{"None: raw transcript", "", false}, {"Clean", "clean", true}, {"Punctuation", "punctuation", false},
			{"English", "english", false}, {"Commit", "commit", false},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v\nwant %v", got, want)
	}
}

func TestMenuChoicesOmitUnavailableSections(t *testing.T) {
	c := config.Defaults()
	c.LLM, c.TTS = nil, nil
	if got := MenuChoices(c); got == nil || len(got["transform"]) != 0 {
		t.Fatalf("choices without [llm]: %v", got)
	}
	c.STT = nil
	delete(c.Providers, "stt")
	if got := MenuChoices(c); len(got["transform"]) != 0 {
		t.Fatalf("STT choices without [stt]: %v", got)
	}
}

func TestMenuChoicesWithoutSTTKeepOtherServices(t *testing.T) {
	c := config.Defaults()
	want := MenuChoices(c)
	for _, key := range []string{"transform", "stt_provider"} {
		delete(want, key)
	}
	c.STT = nil
	delete(c.Providers, "stt")
	if got := MenuChoices(c); !reflect.DeepEqual(got, want) {
		t.Fatalf("choices without [stt]: %v, want %v", got, want)
	}
}

func TestMenuChoicesListProviders(t *testing.T) {
	c := config.Defaults()
	if want := []Choice{{"openai", "openai", true}, {"gemini", "gemini", false}}; !reflect.DeepEqual(MenuChoices(c)["stt_provider"], want) {
		t.Fatalf("stt_provider: %+v", MenuChoices(c)["stt_provider"])
	}
	for _, key := range []string{"language", "tts_provider", "live_provider"} { // the tray only dictates
		if _, ok := MenuChoices(c)[key]; ok {
			t.Fatalf("%s listed", key)
		}
	}
	c.Providers["stt"] = config.Providers{Active: "solo", Names: []string{"solo"}}
	if _, ok := MenuChoices(c)["stt_provider"]; ok {
		t.Fatal("a single provider needs no list")
	}
}

func TestPresetLabel(t *testing.T) {
	for in, want := range map[string]string{"clean": "Clean", "énfasis": "Énfasis", "COMMIT": "Commit", "": ""} {
		if got := presetLabel(in); got != want {
			t.Errorf("presetLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMenuChoicesFollowSavedChoices(t *testing.T) {
	testenv.Setup(t)
	loaded(t)
	config.SaveChoice("transform", "")
	got := MenuChoices(loaded(t))
	if want := (Choice{"None: raw transcript", "", true}); got["transform"][0] != want {
		t.Errorf("transform[0] = %v", got["transform"][0])
	}
}

func TestSecondLockFailsUntilTheFirstIsReleased(t *testing.T) {
	testenv.Setup(t)
	first, err := AcquireLock()
	if err != nil || first == nil {
		t.Fatalf("first lock: %v, %v", first, err)
	}
	if second, err := AcquireLock(); second != nil || err != nil {
		t.Fatalf("second lock: %v, %v", second, err)
	}
	first.Close()
	third, err := AcquireLock()
	if err != nil || third == nil {
		t.Fatalf("lock after release: %v, %v", third, err)
	}
	third.Close()
}

func TestEachStateHasASymbolicSVG(t *testing.T) {
	for _, state := range []string{"idle", "recording", "busy", "failed"} {
		data, err := Icons.ReadFile("icons/habla-" + state + "-symbolic.svg")
		if err != nil {
			t.Fatal(err)
		}
		var root struct{ XMLName xml.Name }
		if err := xml.Unmarshal(data, &root); err != nil || root.XMLName != (xml.Name{Space: "http://www.w3.org/2000/svg", Local: "svg"}) {
			t.Errorf("%s: root = %v, err = %v", state, root.XMLName, err)
		}
	}
}

func withState(state string) { readState = func() string { return state } }

func TestEnqueueListen(t *testing.T) {
	t.Cleanup(func() { readState = rundir.ReadState })
	listen := func(flag string) []string { return []string{HablaPath(), "listen", flag} }

	// A retry ignores input instead of recording after completion.
	var queue [][]string
	withState("failed")
	if !EnqueueListen(&queue, "--retry") {
		t.Fatal("retry not queued")
	}
	if EnqueueListen(&queue, "--start") { // before the retry child has published busy
		t.Fatal("start accepted behind a retry")
	}
	withState("busy")
	for _, flag := range []string{"--stop", "--toggle", "--cancel", "--retry"} {
		if EnqueueListen(&queue, flag) {
			t.Fatalf("%s accepted while busy", flag)
		}
	}
	queue = queue[1:] // the retry exits: no delayed recording is left to spawn
	if len(queue) != 0 {
		t.Fatalf("queue = %v", queue)
	}
	withState("idle")
	if !EnqueueListen(&queue, "--start") || !reflect.DeepEqual(queue, [][]string{listen("--start")}) {
		t.Fatalf("queue = %v", queue)
	}

	// A busy session takes another part (--start, --toggle) and a cancel, never a retry.
	queue = nil
	withState("busy")
	if EnqueueListen(&queue, "--retry") {
		t.Fatal("retry accepted while busy")
	}
	if !EnqueueListen(&queue, "--start") || EnqueueListen(&queue, "--stop") {
		t.Fatal("start/stop not queued as one hold")
	}
	if want := [][]string{listen("--start"), listen("--stop")}; !reflect.DeepEqual(queue, want) {
		t.Fatalf("queue = %v, want %v", queue, want)
	}
	queue = nil
	if !EnqueueListen(&queue, "--cancel") {
		t.Fatal("cancel not run while busy")
	}

	// Transform also accepts cancellation, but never a retry.
	queue = nil
	withState("transform")
	if EnqueueListen(&queue, "--retry") {
		t.Fatal("retry accepted during transform")
	}
	if !EnqueueListen(&queue, "--cancel") {
		t.Fatal("cancel not queued during transform")
	}

	// An immediate stop or cancel waits for startup.
	withState("idle")
	for _, flag := range []string{"--stop", "--cancel"} {
		queue = nil
		if !EnqueueListen(&queue, "--start") {
			t.Fatal("start not queued")
		}
		if EnqueueListen(&queue, "--retry") { // cannot obstruct the pending stop/cancel
			t.Fatal("retry accepted behind a start")
		}
		if EnqueueListen(&queue, flag) { // only the first command spawns now
			t.Fatal(flag + " spawned a second child")
		}
		if want := [][]string{listen("--start"), listen(flag)}; !reflect.DeepEqual(queue, want) {
			t.Fatalf("queue = %v, want %v", queue, want)
		}
	}
}

func TestInstallAutostart(t *testing.T) {
	env := testenv.Setup(t)
	path, err := InstallAutostart()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(env.Dir, "config", "autostart", "habla.desktop"); path != want {
		t.Errorf("path = %s, want %s", path, want)
	}
	data, _ := os.ReadFile(path)
	text := string(data)
	if !strings.HasPrefix(text, "[Desktop Entry]\n") || !strings.Contains(text, "\nType=Application\n") ||
		!strings.Contains(text, "\nExec="+HablaPath()+" tray --foreground\n") {
		t.Errorf("autostart = %s", text)
	}
}

func TestInstallDesktopEntry(t *testing.T) {
	env := testenv.Setup(t)
	t.Setenv("XDG_DATA_HOME", filepath.Join(env.Dir, "data"))
	path, err := InstallDesktopEntry()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(env.Dir, "data", "applications", "io.github.ackstorm.Habla.desktop"); path != want {
		t.Errorf("path = %s, want %s", path, want)
	}
	data, _ := os.ReadFile(path)
	text := string(data)
	if !strings.HasPrefix(text, "[Desktop Entry]\n") || !strings.Contains(text, "\nName=Habla\n") ||
		!strings.Contains(text, "\nExec="+HablaPath()+" tray --foreground\n") || strings.Contains(text, "NoDisplay") {
		t.Errorf("desktop entry = %s", text)
	}
	if again, err := InstallDesktopEntry(); err != nil || again != path { // rewritten on every start
		t.Errorf("second install = %s, %v", again, err)
	}
}

func TestHablaPathIsAbsolute(t *testing.T) {
	if !filepath.IsAbs(HablaPath()) {
		t.Errorf("HablaPath() = %s", HablaPath())
	}
}

func TestPortalDesktop(t *testing.T) {
	for _, c := range []struct {
		session, display, current string
		want                      bool
	}{
		{"wayland", "wayland-0", "KDE", true},
		{"wayland", "wayland-0", "GNOME", true},
		{"wayland", "wayland-0", "ubuntu:GNOME", true},
		{"wayland", "", "KDE", true}, // XDG_SESSION_TYPE alone says Wayland
		{"wayland", "wayland-0", "sway", false},
		{"wayland", "wayland-0", "Hyprland", false},
		{"wayland", "wayland-0", "", false},
		{"x11", "", "KDE", false}, // X11 grabs the key itself
		{"x11", "", "GNOME", false},
	} {
		testenv.Setup(t)
		t.Setenv("XDG_SESSION_TYPE", c.session)
		t.Setenv("WAYLAND_DISPLAY", c.display)
		t.Setenv("XDG_CURRENT_DESKTOP", c.current)
		if got := PortalDesktop(); got != c.want {
			t.Errorf("%+v: got %v", c, got)
		}
	}
}

// wav writes a 16 kHz mono s16 WAV, as the recorder writes it.
func wav(t *testing.T, name string, samples []int16) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	data := make([]byte, 44+2*len(samples)) // the header's contents don't matter to Levels
	for i, s := range samples {
		binary.LittleEndian.PutUint16(data[44+2*i:], uint16(s))
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func repeat(v int16, n int) []int16 {
	s := make([]int16, n)
	for i := range s {
		s[i] = v
	}
	return s
}

func near(t *testing.T, got []float64, want float64, n int) {
	t.Helper()
	if len(got) != n {
		t.Fatalf("len = %d, want %d", len(got), n)
	}
	for _, g := range got {
		if math.Abs(g-want) > 0.01 {
			t.Fatalf("got %v, want %v each", got, want)
		}
	}
}

func TestLevelsOfSilenceQuietAndLoud(t *testing.T) {
	near(t, Levels(wav(t, "silent.wav", repeat(0, 16000)), 9, 0.3), 0, 9)
	near(t, Levels(wav(t, "quiet.wav", repeat(328, 16000)), 9, 0.3), 0.2, 9) // -40 dBFS
	loud := make([]int16, 16000)
	for i := range loud {
		loud[i] = 16000 * int16(1-2*(i%2))
	}
	near(t, Levels(wav(t, "loud.wav", loud), 4, 0.3), 0.875, 4) // about -6 dBFS, not clipping
}

func TestLevelsReadOnlyTheTail(t *testing.T) {
	near(t, Levels(wav(t, "current.wav", append(repeat(16000, 16000), repeat(0, 4800)...)), 9, 0.3), 0, 9)
}

func TestLevelsKeepBriefPeaksInTimeOrder(t *testing.T) {
	samples := repeat(0, 3840) // twelve 20 ms bins at 16 kHz
	samples[0], samples[6*320], samples[11*320] = 32767, -32768, 328
	got := Levels(wav(t, "peaks.wav", samples), 12, 0.24)
	want := []float64{1, 0, 0, 0, 0, 0, 1, 0, 0, 0, 0, 0.2}
	for i, level := range got {
		if math.Abs(level-want[i]) > 0.01 {
			t.Fatalf("brief peaks = %v, want %v", got, want)
		}
	}
}

func TestLevelsDistinguishLoudSpeechFromClipping(t *testing.T) {
	loud := Levels(wav(t, "loud.wav", repeat(16000, 16000)), 12, 0.24)
	clipped := Levels(wav(t, "clipped.wav", repeat(-32768, 16000)), 12, 0.24)
	if loud[11] >= 0.98 || clipped[11] < 0.98 {
		t.Fatalf("ordinary speech and near-clipping need different levels: loud %v, clipped %v", loud, clipped)
	}
}

func TestLevelsIgnoreASampleBeingWritten(t *testing.T) {
	path := wav(t, "current.wav", repeat(16000, 16000))
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.Write([]byte{0}) // half a sample: the recorder is mid-write
	f.Close()
	near(t, Levels(path, 9, 0.3), 0.875, 9)
}

func TestLevelsWithoutAudio(t *testing.T) {
	near(t, Levels(filepath.Join(t.TempDir(), "missing.wav"), 9, 0.3), 0, 9)
	near(t, Levels(wav(t, "empty.wav", nil), 9, 0.3), 0, 9)
	got := Levels(wav(t, "short.wav", repeat(16000, 3)), 9, 0.3)
	want := []float64{0, 0, 0, 0, 0, 0, 0, 0, 0.875}
	for i := range want {
		if math.Abs(got[i]-want[i]) > 0.01 {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestPillHint(t *testing.T) {
	if got, want := PillHint("recording", "Super+Space", true), []Segment{
		{"Release ", false}, {"Super+Space", true}, {" to stop · ", false}, {"Esc", true}, {" discard", false}}; !reflect.DeepEqual(got, want) {
		t.Errorf("held key: %v", got)
	}
	for _, key := range []string{"", "Super+Space"} {
		if got, want := PillHint("recording", key, false), []Segment{{"Stop or discard from the tray", false}}; !reflect.DeepEqual(got, want) {
			t.Errorf("not held, key %q: %v", key, got)
		}
	}
	if got, want := PillHint("recording", "", true), []Segment{{"Release the shortcut to stop · discard from the tray", false}}; !reflect.DeepEqual(got, want) {
		t.Errorf("portal: %v", got)
	}
	for _, phase := range []string{"busy", "transform"} {
		hint := PillHint(phase, "Super+Space", false)
		label := "Transcribing…"
		if phase == "transform" {
			label = "Transforming…"
		}
		if len(hint) != 4 || hint[0].Text != label || hint[2] != (Segment{"Esc", true}) {
			t.Errorf("%s must identify the phase and allow cancellation: %v", phase, hint)
		}
		if got := PillHint(phase, "", false); !reflect.DeepEqual(got, []Segment{{label, false}}) {
			t.Errorf("%s without a hotkey: %v", phase, got)
		}
	}
	for _, segment := range PillHint("copy_failed", "Super+Space", true) {
		if segment.Key || strings.Contains(segment.Text, "Release") || strings.Contains(segment.Text, "discard") {
			t.Errorf("copy failure must not advertise unavailable recording/cancel controls: %v", segment)
		}
	}
}

func TestRecordingSecondsCountFromTheStateFile(t *testing.T) {
	testenv.Setup(t)
	if got := RecordingSeconds(); got != 0 {
		t.Fatalf("no state file: %v", got)
	}
	rundir.WriteState("recording")
	started := time.Now().Add(-7 * time.Second)
	if err := os.Chtimes(filepath.Join(rundir.Dir(), "state"), started, started); err != nil {
		t.Fatal(err)
	}
	if got := RecordingSeconds(); got < 7 || got >= 8 {
		t.Errorf("got %v", got)
	}
}

func TestPhaseLabelRunsUntilDeliveryBegins(t *testing.T) {
	since := time.Now()
	for _, tc := range []struct {
		until, now time.Time
		want       string
	}{
		{time.Time{}, since.Add(840 * time.Millisecond), "(0.8s)"},
		{since.Add(1260 * time.Millisecond), since.Add(time.Minute), "(1.3s)"},
	} {
		if got := PhaseLabel(since, tc.until, tc.now); got != tc.want {
			t.Errorf("got %q, want %q", got, tc.want)
		}
	}
	if got := PhaseLabel(time.Time{}, time.Time{}, since); got != "" {
		t.Errorf("no start: %q", got)
	}
}

func TestPlacePill(t *testing.T) {
	one := []Rect{{0, 0, 1920, 1080}}
	two := []Rect{{0, 0, 1920, 1080}, {1920, 0, 2560, 1440}}
	twoAreas := []Rect{{0, 32, 1920, 1048}, {1920, 0, 2560, 1400}} // a top panel on the first, a bottom one on the second
	size := Size{400, 44}
	for name, tc := range map[string]struct {
		monitors, workareas []Rect
		active              *Rect
		pointer             Point
		x, y                int
	}{
		"one monitor":                       {one, one, &Rect{100, 100, 800, 600}, Point{}, 760, 1080 - 44 - 48},
		"active on the second":              {two, twoAreas, &Rect{2000, 100, 1000, 800}, Point{10, 10}, 1920 + 1080, 1400 - 44 - 48},
		"active centre decides":             {two, twoAreas, &Rect{1000, 100, 1000, 800}, Point{3000, 10}, 760, 32 + 1048 - 44 - 48},
		"no active window: pointer":         {two, twoAreas, nil, Point{3000, 500}, 1920 + 1080, 1400 - 44 - 48},
		"pointer on the first":              {two, twoAreas, nil, Point{5, 5}, 760, 32 + 1048 - 44 - 48},
		"outside every monitor: nearest":    {two, twoAreas, nil, Point{-100, 1200}, 760, 32 + 1048 - 44 - 48},
		"no monitors (lid closed, hotplug)": {nil, nil, &Rect{100, 100, 800, 600}, Point{}, 0, 0},
	} {
		if x, y := PlacePill(tc.monitors, tc.workareas, tc.active, tc.pointer, size); x != tc.x || y != tc.y {
			t.Errorf("%s: (%d, %d), want (%d, %d)", name, x, y, tc.x, tc.y)
		}
	}
}

func TestPillLabels(t *testing.T) {
	for _, tt := range []struct {
		phase, hotkey           string
		held                    bool
		title, subtitle, cancel string
	}{
		{"recording", "Super+Space", true, "Listening", "Release to transcribe…", "ESC"},
		{"recording", "Super+Space", false, "Listening", "Stop from the tray", "Tray"},
		{"recording", "", true, "Listening", "Release to transcribe…", "Tray"},
		{"busy", "Super+Space", false, "Transcribing", "Turning your words into text…", "ESC"},
		{"transform", "Super+Space", false, "Refining text", "Corrections and style", "ESC"},
		{"busy", "", false, "Transcribing", "Turning your words into text…", ""},
		{"transform", "", false, "Refining text", "Corrections and style", ""},
		{"copy_failed", "Super+Space", false, "Copy failed", "See notification", ""},
	} {
		p := pill{phase: tt.phase, hint: PillHint(tt.phase, tt.hotkey, tt.held)}
		title, subtitle, cancel := p.labels()
		if title != tt.title || subtitle != tt.subtitle || cancel != tt.cancel {
			t.Errorf("%s/%s/%t: got %q / %q / %q", tt.phase, tt.hotkey, tt.held, title, subtitle, cancel)
		}
	}
	for _, phase := range []string{"busy", "transform"} {
		p := pill{phase: phase, queued: true}
		if _, subtitle, _ := p.labels(); subtitle != willSend {
			t.Errorf("%s after Enter: %q", phase, subtitle)
		}
	}
}

func TestEnqueueListenPutsPasteFlagsBeforeTheMode(t *testing.T) {
	t.Cleanup(func() { readState = rundir.ReadState })
	withState("idle")
	var queue [][]string
	EnqueueListen(&queue, "--start", "--paste_enter")
	if want := []string{HablaPath(), "listen", "--paste_enter", "--start"}; !reflect.DeepEqual(queue[0], want) {
		t.Fatalf("%q, want %q", queue[0], want)
	}
}
