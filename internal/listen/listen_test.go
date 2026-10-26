package listen

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/ackstorm/habla/internal/audio"
	"github.com/ackstorm/habla/internal/config"
	"github.com/ackstorm/habla/internal/desktop"
	"github.com/ackstorm/habla/internal/logs"
	"github.com/ackstorm/habla/internal/rundir"
	"github.com/ackstorm/habla/internal/stats"
	"github.com/ackstorm/habla/internal/testenv"
)

func TestMain(m *testing.M) {
	testenv.Fake()
	if filepath.Base(os.Args[0]) == "habla" { // the background recording that --start runs (listen.Self)
		os.Exit(Main(os.Args[2:]))
	}
	os.Exit(m.Run())
}

type note struct {
	summary, body string
	desktop       bool
}

// setup is a private environment whose config points at a fake gateway; it records notifications and makes
// --start run the test binary through the link named habla.
func setup(t *testing.T, r testenv.Replies, conf string) (*testenv.Env, *testenv.Gateway, func() []note) {
	t.Helper()
	e := testenv.Setup(t)
	g := testenv.NewGateway(t, r)
	e.WriteConfig(t, g, conf)
	var mu sync.Mutex
	var notes []note
	savedNotify, savedSelf := desktop.Notify, Self
	desktop.Notify = func(s, b string, d bool) { mu.Lock(); notes = append(notes, note{s, b, d}); mu.Unlock() }
	Self = func() (string, error) { return filepath.Join(e.Bin, "habla"), nil }
	t.Cleanup(func() { desktop.Notify, Self = savedNotify, savedSelf })
	return e, g, func() []note { mu.Lock(); defer mu.Unlock(); return slices.Clone(notes) }
}

func want[T comparable](t *testing.T, got []T, want ...T) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func file(name string) string { return filepath.Join(rundir.Dir(), name) }

func exists(name string) bool { _, err := os.Stat(file(name)); return err == nil }

// recorded copies the hello clip into the runtime directory as name.
func recorded(t *testing.T, name string) string {
	t.Helper()
	if err := os.WriteFile(file(name), testenv.HelloWAV(), 0o600); err != nil {
		t.Fatal(err)
	}
	return file(name)
}

func load(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// runProcess delivers the hello clip as a one-part dictation, as a session does after the release.
func runProcess(t *testing.T) int {
	t.Helper()
	return runWith(t, load(t))
}

// runPasting is runProcess with --paste.
func runPasting(t *testing.T) int {
	t.Helper()
	cfg := load(t)
	cfg.Paste = true
	return runWith(t, cfg)
}

func runWith(t *testing.T, cfg *config.Config) int {
	t.Helper()
	wav := recorded(t, "current.wav")
	d := &dictation{cfg: cfg}
	d.add(wav, startUpload(cfg, wav))
	code, _ := d.finish()
	return code
}

// parts is a dictation of the hello clip recorded n times (part-1.wav …).
func parts(t *testing.T, n int, signals, more <-chan os.Signal) *dictation {
	t.Helper()
	cfg := load(t)
	d := &dictation{cfg: cfg, signals: signals, more: more}
	for i := 1; i <= n; i++ {
		wav := recorded(t, fmt.Sprintf("part-%d.wav", i))
		d.add(wav, startUpload(cfg, wav))
	}
	return d
}

type finished struct {
	code  int
	again bool
}

// finishing runs d.finish in the background.
func finishing(d *dictation) <-chan finished {
	c := make(chan finished, 1)
	go func() { code, again := d.finish(); c <- finished{code, again} }()
	return c
}

func result(t *testing.T, c <-chan finished) finished {
	t.Helper()
	select {
	case r := <-c:
		return r
	case <-time.After(10 * time.Second):
		t.Fatal("finish did not return")
		return finished{}
	}
}

// release closes c once, now or at the end of the test, before the fake gateway's server closes (its Close waits for
// held requests): call it after setup.
func release(t *testing.T, c chan struct{}) func() {
	f := sync.OnceFunc(func() { close(c) })
	t.Cleanup(f)
	return f
}

func TestPartsAreJoinedAndTransformedOnce(t *testing.T) {
	e, g, notes := setup(t, testenv.DefaultReplies, conf)
	if r := result(t, finishing(parts(t, 2, nil, nil))); r != (finished{0, false}) {
		t.Fatal(r)
	}
	want(t, e.Lines("copied"), "Hola.")
	want(t, notes(), note{"Copied 1 word", "gpt-4o-mini-transcribe · clean", false})
	want(t, g.Paths(), "/v1/audio/transcriptions", "/v1/audio/transcriptions", "/v1/chat/completions")
	if body := string(g.Body(2)); !strings.Contains(body, "Eh, pues hola. Eh, pues hola.") {
		t.Fatal(body)
	}
	emptyRunDir(t)
}

func TestAnotherPartDropsTheTransformInFlight(t *testing.T) {
	r := testenv.DefaultReplies
	r.HoldChat = make(chan struct{})
	e, g, _ := setup(t, r, conf)
	free := release(t, r.HoldChat)
	more := make(chan os.Signal, 1)
	d := parts(t, 1, nil, more)
	c := finishing(d)
	testenv.WaitFor(t, "the transform request", func() bool { return slices.Contains(g.Paths(), "/v1/chat/completions") })
	if state, _ := os.ReadFile(file("state")); string(state) != "transform\n" {
		t.Errorf("transform request must publish its own phase, got %q", state)
	}
	more <- syscall.SIGUSR2
	if got := result(t, c); got != (finished{0, true}) {
		t.Fatal(got)
	}
	want(t, e.Lines("copied"))
	wav := recorded(t, "part-2.wav")
	d.add(wav, startUpload(d.cfg, wav))
	free()
	if got := result(t, finishing(d)); got != (finished{0, false}) {
		t.Fatal(got)
	}
	want(t, e.Lines("copied"), "Hola.")
	paths := g.Paths()
	if body := string(g.Body(len(paths) - 1)); !strings.Contains(body, "Eh, pues hola. Eh, pues hola.") {
		t.Fatal(body)
	}
	emptyRunDir(t)
}

func TestCancelWhileBusyDiscardsEveryPart(t *testing.T) {
	r := testenv.DefaultReplies
	r.HoldSTT = make(chan struct{})
	e, g, _ := setup(t, r, conf)
	release(t, r.HoldSTT) // after setup: cleanups run last-in first-out, and the server's Close waits for held requests
	signals := make(chan os.Signal, 2)
	c := finishing(parts(t, 2, signals, nil))
	testenv.WaitFor(t, "both transcriptions", func() bool { return len(g.Paths()) == 2 })
	signals <- syscall.SIGUSR1 // a late --stop: nothing to stop
	signals <- syscall.SIGTERM
	if got := result(t, c); got != (finished{1, false}) {
		t.Fatal(got)
	}
	want(t, e.Lines("copied"))
	emptyRunDir(t)
}

func TestFailedPartsAreJoinedForRetry(t *testing.T) {
	r := testenv.DefaultReplies
	r.STTStatus = 403
	setup(t, r, conf)
	if got := result(t, finishing(parts(t, 2, nil, nil))); got != (finished{1, false}) {
		t.Fatal(got)
	}
	pcm := testenv.HelloPCM(t)
	saved, _ := os.ReadFile(file("failed.wav"))
	if !bytes.Equal(saved, append(audio.Header(int64(2*len(pcm))), append(pcm, pcm...)...)) {
		t.Fatal("failed.wav is not both parts' PCM")
	}
	if exists("part-1.wav") || exists("part-2.wav") || rundir.ReadState() != "failed" {
		t.Fatal("parts left, or not failed")
	}
}

// holder makes a live habla process hold listen.pid, with state.
func holder(t *testing.T, state string) {
	t.Helper()
	cmd := testenv.HablaProcess(t)
	os.WriteFile(file(rundir.Listen), []byte(fmt.Sprintf("%d\n", cmd.Process.Pid)), 0o644)
	rundir.WriteState(state)
}

// interactive runs `habla listen args…` with a pty as stdin; keys types into it and wait returns the exit code.
func interactive(t *testing.T, args ...string) (keys func(string), wait func() int) {
	t.Helper()
	master, slave := testenv.Pty(t)
	saved := os.Stdin
	os.Stdin = slave
	t.Cleanup(func() { os.Stdin = saved })
	code := make(chan int, 1)
	go func() { code <- Main(args) }()
	return func(k string) { master.WriteString(k) }, func() int {
		select {
		case c := <-code:
			return c
		case <-time.After(10 * time.Second):
			t.Fatal("habla listen did not finish")
			return 0
		}
	}
}

// recording waits until the interactive run holds listen.pid (it cannot be seen as "recording" through
// rundir.ReadState: the test binary is not named habla) and its recorder has created the WAV (its SIGINT handler
// is installed by then: an earlier SIGINT would kill the fake).
// recording waits for the session's recorder to have written a dictation's worth (minRecording) of audio.
func recording(t *testing.T) {
	t.Helper()
	testenv.WaitFor(t, "listen.pid", func() bool {
		info, err := os.Stat(file("current.wav"))
		return exists(rundir.Listen) && err == nil && info.Size() >= int64(len(audio.Header(0))+minRecording)
	})
}

func emptyRunDir(t *testing.T) {
	t.Helper()
	entries, _ := os.ReadDir(rundir.Dir())
	if len(entries) != 0 {
		t.Fatalf("left in the runtime directory: %v", entries)
	}
}

const conf = "[general]\nmax_seconds = 30\n"

// --- pipeline -----------------------------------------------------------------------------------

func TestPipelineTranscribesTransformsWithCleanAndCopies(t *testing.T) {
	e, g, notes := setup(t, testenv.DefaultReplies, conf)
	if code := runProcess(t); code != 0 {
		t.Fatal(code)
	}
	want(t, e.Lines("copied"), "Hola.")
	want(t, notes(), note{"Copied 1 word", "gpt-4o-mini-transcribe · clean", false})
	want(t, g.Paths(), "/v1/audio/transcriptions", "/v1/chat/completions")
	pcm := testenv.HelloPCM(t)
	start, end := audio.Trim(pcm, -45)
	if !bytes.Equal(g.Audio()[0], append(audio.Header(int64(end-start)), pcm[start:end]...)) {
		t.Fatal("the upload is not the trimmed recording")
	}
	if exists("current.wav") || rundir.ReadState() != "idle" {
		t.Fatal("recording or state left behind")
	}
}

func TestPresetNoneCopiesTheRawText(t *testing.T) {
	e, g, notes := setup(t, testenv.DefaultReplies, conf+"[stt.providers.openai]\ntransform = \"\"\n")
	runProcess(t)
	want(t, e.Lines("copied"), "Eh, pues hola.")
	want(t, g.Paths(), "/v1/audio/transcriptions")
	want(t, notes(), note{"Copied 3 words", "gpt-4o-mini-transcribe", false})
}

func TestNoLLMCopiesRawTextWithoutTransformRequest(t *testing.T) {
	e, g, notes := setup(t, testenv.DefaultReplies, conf)
	text := strings.ReplaceAll(config.DefaultTOML, `"https://api.openai.com/v1"`, `"`+g.URL+`"`)
	text = strings.ReplaceAll(text, `"env:OPENAI_API_KEY"`, `"env:HABLA_API_KEY"`)
	start, end := strings.Index(text, "\n[llm]"), strings.Index(text, "\n[stt]")
	e.WriteConfigText(t, text[:start]+text[end:])
	if code := runProcess(t); code != 0 {
		t.Fatal(code)
	}
	want(t, e.Lines("copied"), "Eh, pues hola.")
	want(t, g.Paths(), "/v1/audio/transcriptions")
	want(t, notes(), note{"Copied 3 words", "gpt-4o-mini-transcribe", false})
}

func TestTransformFailureCopiesTheRawTextAndNeverPastes(t *testing.T) {
	r := testenv.DefaultReplies
	r.ChatStatus = 500
	e, _, notes := setup(t, r, conf)
	if code := runPasting(t); code != 0 {
		t.Fatal(code)
	}
	want(t, e.Lines("copied"), "Eh, pues hola.")
	want(t, e.Lines("pasted"))
	want(t, notes(), note{"Transform failed: raw text copied", "Gateway 500: transform down", false})
}

func TestEmptyTransformCopiesTheRawTextAndNeverPastes(t *testing.T) {
	for _, transformed := range []string{"", " \n\t "} {
		t.Run(fmt.Sprintf("%q", transformed), func(t *testing.T) {
			r := testenv.DefaultReplies
			r.Transformed = transformed
			e, _, notes := setup(t, r, conf)
			if code := runPasting(t); code != 0 {
				t.Fatal(code)
			}
			want(t, e.Lines("copied"), r.Transcript)
			want(t, e.Lines("pasted"))
			want(t, notes(), note{"Transform failed: raw text copied", "Gateway reply has no transform text", false})
		})
	}
}

func TestTranscriptionFailureKeepsTheAudioForRetry(t *testing.T) {
	r := testenv.DefaultReplies
	r.STTStatus = 403
	e, _, notes := setup(t, r, conf)
	if code := runProcess(t); code != 1 {
		t.Fatal(code)
	}
	want(t, notes(), note{"Gateway 403: no access", RetryHint, false})
	saved, _ := os.ReadFile(file("failed.wav"))
	original := testenv.HelloWAV()
	if !bytes.Equal(saved, original) || exists("current.wav") || rundir.ReadState() != "failed" {
		t.Fatal("the untrimmed recording must be kept as failed.wav")
	}
	want(t, e.Lines("copied"))
}

func TestMissingKeySendsNothingAndKeepsTheAudio(t *testing.T) {
	_, g, notes := setup(t, testenv.DefaultReplies, conf)
	t.Setenv("HABLA_API_KEY", "")
	runProcess(t)
	want(t, notes(), note{"No API key: $HABLA_API_KEY is not set.", RetryHint, false})
	want(t, g.Paths())
	if !exists("failed.wav") {
		t.Fatal("failed.wav missing")
	}
}

func TestEmptyTranscriptCopiesNothing(t *testing.T) {
	r := testenv.DefaultReplies
	r.Transcript = "  "
	e, g, notes := setup(t, r, conf)
	if code := runProcess(t); code != 1 {
		t.Fatal(code)
	}
	want(t, notes(), note{"Nothing heard.", "", false})
	want(t, e.Lines("copied"))
	want(t, g.Paths(), "/v1/audio/transcriptions")
}

func TestTapStreamsNothing(t *testing.T) {
	e, g, _ := setup(t, testenv.DefaultReplies, conf)
	wav := filepath.Join(e.Dir, "tap.wav")
	pcm := testenv.HelloPCM(t)[:minRecording-audio.FrameBytes]
	if err := os.WriteFile(wav, append(audio.Header(int64(len(pcm))), pcm...), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HABLA_FAKE_RECORDING", wav)
	keys, wait := interactive(t)
	testenv.WaitFor(t, "the whole recording", func() bool { return len(e.Lines("recorded")) > 0 })
	keys("\n")
	if code := wait(); code != 1 {
		t.Fatal(code)
	}
	want(t, g.Paths())
}

func TestCopyFailureShowsTheText(t *testing.T) {
	_, _, notes := setup(t, testenv.DefaultReplies, conf)
	t.Setenv("HABLA_FAKE_FAIL", "wl-copy")
	if code := runProcess(t); code != 1 {
		t.Fatal(code)
	}
	want(t, notes(), note{"Copy failed: wl-copy failed", "Hola.", false})
}

func TestPasteOption(t *testing.T) {
	e, _, notes := setup(t, testenv.DefaultReplies, conf)
	runPasting(t)
	want(t, e.Lines("pasted"), "wtype -M ctrl v -m ctrl")
	want(t, notes(), note{"Pasted", "gpt-4o-mini-transcribe · clean", false})
}

func TestDeliveryEndsBeforeTheNotificationReturns(t *testing.T) {
	setup(t, testenv.DefaultReplies, conf)
	notifying, held := make(chan struct{}), make(chan struct{})
	free := release(t, held)
	desktop.Notify = func(string, string, bool) { close(notifying); <-held }
	done := finishing(parts(t, 1, nil, nil))
	select {
	case <-notifying:
	case <-time.After(5 * time.Second):
		t.Fatal("notification not reached")
	}
	if exists("state") {
		t.Fatal("clipboard delivery is finished; a slow notification must not keep the pill busy")
	}
	free()
	if got := result(t, done); got != (finished{0, false}) {
		t.Fatal(got)
	}
}

func TestDeliveryRemainsVisibleUntilPasteAndDoesNotBlockANewRecording(t *testing.T) {
	e, _, _ := setup(t, testenv.DefaultReplies, conf)
	copyGate, pasteGate := filepath.Join(e.Dir, "copy-gate"), filepath.Join(e.Dir, "paste-gate")
	for _, path := range []string{copyGate, pasteGate} {
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Remove(path) })
	}
	t.Setenv("HABLA_FAKE_COPY_GATE", copyGate)
	t.Setenv("HABLA_FAKE_PASTE_GATE", pasteGate)
	if code := Main([]string{"--start", "--paste"}); code != 0 {
		t.Fatal(code)
	}
	pid := rundir.RunningPID(rundir.Listen)
	testenv.WaitFor(t, "the recording", func() bool { return len(e.Lines("recorded")) == 1 })
	if code := Main([]string{"--stop"}); code != 0 {
		t.Fatal(code)
	}
	testenv.WaitFor(t, "copy started", func() bool { return len(e.Lines("copy-started")) == 1 })
	if got := rundir.ReadState(); got != "copying" || rundir.RunningPID(rundir.Listen) != 0 {
		t.Fatalf("clipboard pending: state %q, recording slot must be free", got)
	}
	want(t, e.Lines("copied"))
	if err := os.Remove(copyGate); err != nil {
		t.Fatal(err)
	}
	testenv.WaitFor(t, "paste started", func() bool { return len(e.Lines("paste-started")) == 1 })
	if got := rundir.ReadState(); got != "pasting" {
		t.Fatal(got)
	}
	want(t, e.Lines("copied"), "Hola.")
	want(t, e.Lines("pasted"))
	if code := Main([]string{"--start"}); code != 0 {
		t.Fatal(code)
	}
	if err := os.Remove(pasteGate); err != nil {
		t.Fatal(err)
	}
	testenv.WaitFor(t, "previous delivery to finish", func() bool { return !rundir.Alive(pid) })
	if got := rundir.ReadState(); got != "recording" {
		t.Fatalf("previous delivery erased the new recording: %q", got)
	}
	testenv.WaitFor(t, "new recorder", func() bool { return len(e.Lines("recorded")) == 2 })
	if code := Main([]string{"--cancel"}); code != 0 {
		t.Fatal(code)
	}
	testenv.WaitFor(t, "idle", func() bool { return rundir.ReadState() == "idle" })
}

func TestNoPasteToolFallsBackToTheClipboard(t *testing.T) {
	e, _, notes := setup(t, testenv.DefaultReplies, conf)
	e.Remove("wtype")
	e.Remove("ydotool")
	if code := runPasting(t); code != 0 {
		t.Fatal(code)
	}
	want(t, e.Lines("copied"), "Hola.")
	want(t, notes(), note{"Copied: press Ctrl+V to paste", "no paste tool: install wtype or ydotool 1.x", false})
}

func TestInvalidConfigStopsBeforeRecording(t *testing.T) {
	for _, args := range [][]string{nil, {"--start"}, {"--retry"}} {
		t.Run(fmt.Sprint(args), func(t *testing.T) {
			e, g, _ := setup(t, testenv.DefaultReplies, conf)
			path := filepath.Join(e.Dir, "config", "habla", "config.toml")
			os.WriteFile(path, []byte("[general]\nmax_seconds = \"x\"\n"), 0o644)
			if args != nil && args[0] == "--retry" {
				recorded(t, "failed.wav")
			}
			code := -1
			out := testenv.Stderr(t, func() {
				if args == nil {
					_, wait := interactive(t)
					code = wait()
				} else {
					code = Main(args)
				}
			})
			if code != 1 || !strings.HasPrefix(out, "Invalid config: "+path+": toml: line 2") {
				t.Fatalf("exit %d, stderr %q", code, out)
			}
			want(t, g.Paths()) // a recording would have exited 0 (--start, --toggle) or waited for Enter
		})
	}
}

func TestLoadConfigRequiresSTT(t *testing.T) {
	e, _, _ := setup(t, testenv.DefaultReplies, "")
	e.WriteConfigText(t, "[general]\n")
	cfg, err := loadConfig(false, false)
	if err == nil || cfg != nil || !strings.Contains(err.Error(), "no [stt.providers.<name>] section") {
		t.Fatalf("config=%+v err=%v", cfg, err)
	}
}

func TestDebugLogNeverHasTheKeyOrTheText(t *testing.T) {
	e, _, _ := setup(t, testenv.DefaultReplies, conf)
	keys, wait := interactive(t, "--log", "debug")
	recording(t)
	testenv.WaitFor(t, "the whole recording", func() bool { return e.Lines("recorded") != nil })
	keys("\n")
	if code := wait(); code != 0 {
		t.Fatal(code)
	}
	data, err := os.ReadFile(logs.Path())
	if err != nil {
		t.Fatal(err)
	}
	log := string(data)
	for _, secret := range []string{"sk-test", "Bearer", "Authorization", "hola", "Hola"} {
		if strings.Contains(log, secret) {
			t.Errorf("the log has %q", secret)
		}
	}
	if !strings.Contains(log, " chunk bytes=") || !strings.Contains(log, " DEBUG [delta_ms=") {
		t.Error("no streamed chunk in the debug log")
	}
}

// --- interactive --------------------------------------------------------------------------------

func TestEnterStopsAndCopies(t *testing.T) {
	e, _, _ := setup(t, testenv.DefaultReplies, conf)
	keys, wait := interactive(t)
	recording(t)
	keys("\n")
	if code := wait(); code != 0 {
		t.Fatal(code)
	}
	want(t, e.Lines("copied"), "Hola.")
	want(t, e.Lines("pasted"))
	emptyRunDir(t)
}

func TestPasteFlagPastesAnInteractiveRun(t *testing.T) {
	e, _, _ := setup(t, testenv.DefaultReplies, conf)
	keys, wait := interactive(t, "--paste")
	recording(t)
	keys("\r")
	wait()
	want(t, e.Lines("pasted"), "wtype -M ctrl v -m ctrl")
}

func TestEscOrSIGTERMDiscards(t *testing.T) {
	for name, discard := range map[string]func(keys func(string)){
		"esc":     func(keys func(string)) { keys("\x1b") },
		"sigterm": func(func(string)) { syscall.Kill(os.Getpid(), syscall.SIGTERM) },
	} {
		t.Run(name, func(t *testing.T) {
			e, _, _ := setup(t, testenv.DefaultReplies, conf)
			keys, wait := interactive(t)
			recording(t)
			discard(keys)
			if code := wait(); code != 1 {
				t.Fatal(code)
			}
			want(t, e.Lines("copied"))
			emptyRunDir(t)
		})
	}
}

func TestSIGUSR1StopsAnInteractiveRun(t *testing.T) {
	e, _, _ := setup(t, testenv.DefaultReplies, conf)
	_, wait := interactive(t)
	recording(t)
	syscall.Kill(os.Getpid(), syscall.SIGUSR1)
	if code := wait(); code != 0 {
		t.Fatal(code)
	}
	want(t, e.Lines("copied"), "Hola.")
}

func TestMaxSecondsStopsOnItsOwn(t *testing.T) {
	e, _, _ := setup(t, testenv.DefaultReplies, "[general]\nmax_seconds = 1\n")
	_, wait := interactive(t)
	if code := wait(); code != 0 {
		t.Fatal(code)
	}
	want(t, e.Lines("copied"), "Hola.")
}

func TestRecorderFailureIsReportedAndLeavesIdle(t *testing.T) {
	_, g, notes := setup(t, testenv.DefaultReplies, conf)
	t.Setenv("HABLA_FAKE_FAIL", "pw-record")
	_, wait := interactive(t)
	if code := wait(); code != 1 {
		t.Fatal(code)
	}
	want(t, notes(), note{"Recording failed: no microphone", "", false})
	want(t, g.Paths())
	emptyRunDir(t)
}

func TestSecondListenWhileRecording(t *testing.T) {
	setup(t, testenv.DefaultReplies, conf)
	holder(t, "recording")
	_, wait := interactive(t)
	if code := wait(); code != 1 {
		t.Fatal(code)
	}
	if !exists(rundir.Listen) || rundir.ReadState() != "recording" {
		t.Fatal("the running recording's files were touched")
	}
}

func TestNoTerminalExits2(t *testing.T) {
	setup(t, testenv.DefaultReplies, conf)
	null, _ := os.Open(os.DevNull)
	defer null.Close()
	saved := os.Stdin
	os.Stdin = null
	defer func() { os.Stdin = saved }()
	if code := Main(nil); code != 2 {
		t.Fatal(code)
	}
}

// --- control ------------------------------------------------------------------------------------

func stdout(t *testing.T, f func()) string {
	t.Helper()
	r, w, _ := os.Pipe()
	saved := os.Stdout
	os.Stdout = w
	f()
	os.Stdout = saved
	w.Close()
	out, _ := io.ReadAll(r)
	return string(out)
}

func TestStatus(t *testing.T) {
	setup(t, testenv.DefaultReplies, conf)
	if out := stdout(t, func() { Main([]string{"--status"}) }); out != "idle\n" {
		t.Fatalf("%q", out)
	}
	rundir.WriteState("failed")
	if out := stdout(t, func() { Main([]string{"--status"}) }); out != "failed\n" {
		t.Fatalf("%q", out)
	}
}

func TestStopCancelToggleSignalTheRecordingProcess(t *testing.T) {
	for flag, sig := range map[string]syscall.Signal{"--stop": syscall.SIGUSR1, "--cancel": syscall.SIGTERM, "--toggle": syscall.SIGUSR1} {
		t.Run(flag, func(t *testing.T) {
			setup(t, testenv.DefaultReplies, conf)
			cmd := testenv.HablaProcess(t)
			os.WriteFile(file(rundir.Listen), []byte(fmt.Sprintf("%d\n", cmd.Process.Pid)), 0o644)
			rundir.WriteState("recording")
			if code := Main([]string{flag}); code != 0 {
				t.Fatal(code)
			}
			cmd.Wait()
			if status := cmd.ProcessState.Sys().(syscall.WaitStatus); status.Signal() != sig {
				t.Fatalf("got %v, want %v", status.Signal(), sig)
			}
		})
	}
}

func TestStopAndCancelWhenNotRecording(t *testing.T) {
	setup(t, testenv.DefaultReplies, conf)
	for _, flag := range []string{"--stop", "--cancel"} {
		if code := Main([]string{flag}); code != 1 {
			t.Fatal(flag, code)
		}
	}
}

func TestOnlyOneModeAtATime(t *testing.T) {
	setup(t, testenv.DefaultReplies, conf)
	if code := Main([]string{"--start", "--stop"}); code != 2 {
		t.Fatal(code)
	}
}

func TestRetrySuccessDeletesFailedWav(t *testing.T) {
	e, _, _ := setup(t, testenv.DefaultReplies, conf)
	recorded(t, "failed.wav")
	if code := Main([]string{"--retry"}); code != 0 {
		t.Fatal(code)
	}
	want(t, e.Lines("copied"), "Hola.")
	emptyRunDir(t)
}

func TestRetryWithPaste(t *testing.T) {
	e, _, _ := setup(t, testenv.DefaultReplies, conf)
	recorded(t, "failed.wav")
	Main([]string{"--retry", "--paste"})
	want(t, e.Lines("pasted"), "wtype -M ctrl v -m ctrl")
}

func TestRetryFailureKeepsFailedWav(t *testing.T) {
	r := testenv.DefaultReplies
	r.STTStatus = 500
	setup(t, r, conf)
	recorded(t, "failed.wav")
	if code := Main([]string{"--retry"}); code != 1 {
		t.Fatal(code)
	}
	if !exists("failed.wav") || rundir.ReadState() != "failed" {
		t.Fatal("failed.wav lost")
	}
}

func TestRetryWithNothingToRetry(t *testing.T) {
	setup(t, testenv.DefaultReplies, conf)
	if code := Main([]string{"--retry"}); code != 1 {
		t.Fatal(code)
	}
}

func TestRetryWhileRecording(t *testing.T) {
	setup(t, testenv.DefaultReplies, conf)
	holder(t, "recording")
	recorded(t, "failed.wav")
	if code := Main([]string{"--retry"}); code != 1 {
		t.Fatal(code)
	}
	if !exists("failed.wav") {
		t.Fatal("failed.wav touched")
	}
}

func TestStartThenStopInTheBackground(t *testing.T) {
	for name, tc := range map[string]struct {
		flags  []string
		pasted []string
	}{"plain": {nil, nil}, "paste": {[]string{"--paste"}, []string{"wtype -M ctrl v -m ctrl"}}} {
		t.Run(name, func(t *testing.T) {
			e, _, _ := setup(t, testenv.DefaultReplies, conf)
			if code := Main(append([]string{"--start"}, tc.flags...)); code != 0 {
				t.Fatal(code)
			}
			if pid := rundir.RunningPID(rundir.Listen); pid == 0 || pid == os.Getpid() {
				t.Fatalf("no background recording (pid %d)", pid)
			}
			if rundir.ReadState() != "recording" {
				t.Fatal(rundir.ReadState())
			}
			recording(t) // an earlier SIGINT would kill the fake recorder before it installs its handler
			pid := rundir.RunningPID(rundir.Listen)
			if code := Main([]string{"--stop"}); code != 0 {
				t.Fatal(code)
			}
			// The session releases listen.pid before it copies: wait for it to exit.
			testenv.WaitFor(t, "the background recording to finish", func() bool { return !rundir.Alive(pid) })
			want(t, e.Lines("copied"), "Hola.")
			want(t, e.Lines("pasted"), tc.pasted...)
			emptyRunDir(t)
		})
	}
}

func TestStartReportsARecorderThatCannotStart(t *testing.T) {
	e, _, _ := setup(t, testenv.DefaultReplies, conf)
	e.Remove("pw-record")
	e.Remove("parecord")
	if code := Main([]string{"--start"}); code != 1 {
		t.Fatal(code)
	}
	// The child answers the parent before its deferred cleanup runs.
	testenv.WaitFor(t, "the background process to clean up", func() bool {
		entries, _ := os.ReadDir(rundir.Dir())
		return len(entries) == 0
	})
}

// backgroundPart records the hello clip once more in the background: start, the whole recording, stop. n counts the
// recordings so far, this one included.
func backgroundPart(t *testing.T, e *testenv.Env, n int, start, stop string) {
	t.Helper()
	if code := Main([]string{start}); code != 0 {
		t.Fatal(start, code)
	}
	// An earlier SIGINT would kill the fake recorder before it installs its handler.
	testenv.WaitFor(t, "the whole recording", func() bool { return len(e.Lines("recorded")) == n })
	if code := Main([]string{stop}); code != 0 {
		t.Fatal(stop, code)
	}
}

func TestStartWhileBusyRecordsAnotherPart(t *testing.T) {
	for name, flags := range map[string][3]string{
		"start": {"--start", "--stop", "busy"}, "toggle": {"--toggle", "--toggle", "busy"},
		"transform-start": {"--start", "--stop", "transform"}, "transform-toggle": {"--toggle", "--toggle", "transform"},
	} {
		t.Run(name, func(t *testing.T) {
			r := testenv.DefaultReplies
			gate := make(chan struct{})
			if flags[2] == "transform" {
				r.HoldChat = gate
			} else {
				r.HoldSTT = gate
			}
			e, g, _ := setup(t, r, conf)
			free := release(t, gate)
			backgroundPart(t, e, 1, flags[0], flags[1])
			pid := rundir.RunningPID(rundir.Listen)
			testenv.WaitFor(t, flags[2], func() bool { return rundir.ReadState() == flags[2] })
			backgroundPart(t, e, 2, flags[0], flags[1])
			if got := rundir.RunningPID(rundir.Listen); got != pid {
				t.Fatalf("the second part ran in process %d, not %d", got, pid)
			}
			free()
			testenv.WaitFor(t, "the session to exit", func() bool { return !rundir.Alive(pid) })
			want(t, e.Lines("copied"), "Hola.")
			paths := g.Paths()
			if body := string(g.Body(len(paths) - 1)); !strings.Contains(body, "Eh, pues hola. Eh, pues hola.") {
				t.Fatal(body)
			}
			emptyRunDir(t)
		})
	}
}

func TestCancelWhileBusyInTheBackground(t *testing.T) {
	for _, phase := range []string{"busy", "transform"} {
		t.Run(phase, func(t *testing.T) {
			r := testenv.DefaultReplies
			gate := make(chan struct{})
			if phase == "transform" {
				r.HoldChat = gate
			} else {
				r.HoldSTT = gate
			}
			e, _, _ := setup(t, r, conf)
			release(t, gate)
			backgroundPart(t, e, 1, "--start", "--stop")
			pid := rundir.RunningPID(rundir.Listen)
			testenv.WaitFor(t, phase, func() bool { return rundir.ReadState() == phase })
			if code := Main([]string{"--stop"}); code != 1 { // busy: nothing to stop
				t.Fatal(code)
			}
			if code := Main([]string{"--cancel"}); code != 0 {
				t.Fatal(code)
			}
			testenv.WaitFor(t, "the session to exit", func() bool { return !rundir.Alive(pid) })
			want(t, e.Lines("copied"))
			emptyRunDir(t)
		})
	}
}

func TestSendWhileBusyPastesAndPressesEnter(t *testing.T) {
	r := testenv.DefaultReplies
	gate := make(chan struct{})
	r.HoldSTT = gate
	e, _, _ := setup(t, r, conf)
	open := release(t, gate)
	if code := Main([]string{"--send"}); code != 1 { // idle: nothing to send
		t.Fatal(code)
	}
	backgroundPart(t, e, 1, "--start", "--stop") // no --paste
	pid := rundir.RunningPID(rundir.Listen)
	testenv.WaitFor(t, "busy", func() bool { return rundir.ReadState() == "busy" })
	if code := Main([]string{"--send"}); code != 0 {
		t.Fatal(code)
	}
	open()
	testenv.WaitFor(t, "the session to exit", func() bool { return !rundir.Alive(pid) })
	want(t, e.Lines("copied"), "Hola.")
	want(t, e.Lines("pasted"), "wtype -M ctrl v -m ctrl", "wtype -k Return")
	emptyRunDir(t)
}

func TestSendWhileRecordingPastesAndPressesEnterAfterTheStop(t *testing.T) { // the hotkey's Enter while held
	e, _, _ := setup(t, testenv.DefaultReplies, conf)
	if code := Main([]string{"--start"}); code != 0 {
		t.Fatal(code)
	}
	testenv.WaitFor(t, "the whole recording", func() bool { return len(e.Lines("recorded")) == 1 })
	pid := rundir.RunningPID(rundir.Listen)
	if code := Main([]string{"--send"}); code != 0 {
		t.Fatal(code)
	}
	if code := Main([]string{"--stop"}); code != 0 {
		t.Fatal(code)
	}
	testenv.WaitFor(t, "the session to exit", func() bool { return !rundir.Alive(pid) })
	want(t, e.Lines("pasted"), "wtype -M ctrl v -m ctrl", "wtype -k Return")
	emptyRunDir(t)
}

func TestDictationIsRecordedInStatsWithoutText(t *testing.T) {
	r := testenv.DefaultReplies
	r.Cost = "0.001"
	setup(t, r, conf)
	if code := runProcess(t); code != 0 {
		t.Fatal(code)
	}
	records, err := stats.Load(stats.Path())
	if err != nil || len(records) != 1 {
		t.Fatal(records, err)
	}
	got := records[0]
	var services []string
	for _, c := range got.Calls {
		services = append(services, c.Service)
		if c.Source != "header" || c.Cost != 0.001 {
			t.Fatalf("%+v", c)
		}
	}
	want(t, services, "stt", "llm")
	if _, ok := got.Ms["total"]; got.Kind != "listen" || got.Words != 1 || got.AudioS <= 0 || !ok {
		t.Fatalf("%+v", got)
	}
	data, _ := os.ReadFile(stats.Path())
	if strings.Contains(strings.ToLower(string(data)), "hola") {
		t.Fatal("stats.jsonl holds the dictated text")
	}
}

func TestStartWhileAnotherRecordsWaitsThenFails(t *testing.T) {
	setup(t, testenv.DefaultReplies, conf)
	holder(t, "recording") // never reaches busy
	if code := Main([]string{"--start"}); code != 1 {
		t.Fatal(code)
	}
	if !exists(rundir.Listen) || rundir.ReadState() != "recording" {
		t.Fatal("the running recording's files were touched")
	}
}

const altSTT = "\n[stt]\nprovider = \"openai\"\n[stt.providers.alt]\napi = \"openai\"\napi_key = \"env:HABLA_API_KEY\"\nmodel = \"alt-stt\"\n"

func TestProviderFlagPicksTheSTTProvider(t *testing.T) {
	e, g, _ := setup(t, testenv.DefaultReplies, conf)
	e.WriteConfig(t, g, conf+altSTT+"endpoint = \""+g.URL+"/alt\"\n")
	recorded(t, "failed.wav")
	if code := Main([]string{"--retry", "--provider", "alt"}); code != 0 {
		t.Fatal(code)
	}
	if p := g.Paths(); len(p) == 0 || !strings.HasPrefix(p[0], "/v1/alt/") {
		t.Fatalf("STT request did not use the alt provider: %q", g.Paths())
	}
	recorded(t, "failed.wav")
	if code := Main([]string{"--retry", "--provider", "nope"}); code != 1 {
		t.Fatalf("unknown provider: exit %d", code)
	}
}

func TestTransformFlagPicksTheTransform(t *testing.T) {
	_, g, _ := setup(t, testenv.DefaultReplies, conf)
	recorded(t, "failed.wav")
	if code := Main([]string{"--retry", "--transform", "english"}); code != 0 {
		t.Fatal(code)
	}
	if p := g.Paths(); len(p) != 2 || !strings.Contains(string(g.Body(1)), "Translate to English.") {
		t.Fatalf("the transform was not english: %q\n%s", p, g.Body(1))
	}
	recorded(t, "failed.wav")
	if code := Main([]string{"--retry", "--transform", ""}); code != 0 {
		t.Fatal(code)
	}
	if p := g.Paths(); len(p) != 3 {
		t.Fatalf("--transform \"\" still transformed: %q", p)
	}
	recorded(t, "failed.wav")
	if code := Main([]string{"--retry", "--transform", "nope"}); code != 1 {
		t.Fatalf("unknown transform: exit %d", code)
	}
}

func TestStartPassesTheProviderToTheBackgroundSession(t *testing.T) {
	e, g, _ := setup(t, testenv.DefaultReplies, conf)
	e.WriteConfig(t, g, conf+altSTT+"endpoint = \""+g.URL+"/alt\"\n")
	if code := Main([]string{"--start", "--provider", "alt", "--transform", ""}); code != 0 {
		t.Fatal(code)
	}
	recording(t)
	pid := rundir.RunningPID(rundir.Listen)
	if code := Main([]string{"--stop"}); code != 0 {
		t.Fatal(code)
	}
	testenv.WaitFor(t, "the background recording to finish", func() bool { return !rundir.Alive(pid) })
	if p := g.Paths(); len(p) != 1 || !strings.HasPrefix(p[0], "/v1/alt/") {
		t.Fatalf("want one STT request to the alt provider and no transform: %q", g.Paths())
	}
}
