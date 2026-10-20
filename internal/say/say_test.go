package say

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/ackstorm/habla/internal/desktop"
	"github.com/ackstorm/habla/internal/rundir"
	"github.com/ackstorm/habla/internal/stats"
	"github.com/ackstorm/habla/internal/testenv"
)

func TestMain(m *testing.M) {
	testenv.Fake()
	os.Exit(m.Run())
}

const rewrite = "Ola, ¿qué tá?"

// setup is a private environment whose config has the default [tts.providers.openai] (plus extra lines) and a fake gateway that
// rewrites to `rewrite`; it returns what Notify was asked to print.
func setup(t *testing.T, r testenv.Replies, extra string) (*testenv.Env, *testenv.Gateway, func() []string) {
	t.Helper()
	e := testenv.Setup(t)
	r.Transformed = rewrite
	g := testenv.NewGateway(t, r)
	e.WriteConfig(t, g, "[tts.providers.openai]\n"+extra)
	var mu sync.Mutex
	var notes []string
	saved, savedStdin := desktop.Notify, Stdin
	desktop.Notify = func(s, b string, _ bool) { mu.Lock(); notes = append(notes, strings.TrimSpace(s+"\n"+b)); mu.Unlock() }
	t.Cleanup(func() { desktop.Notify, Stdin = saved, savedStdin })
	return e, g, func() []string { mu.Lock(); defer mu.Unlock(); return slices.Clone(notes) }
}

func want[T comparable](t *testing.T, got []T, want ...T) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func runtimeFiles(t *testing.T) []string {
	t.Helper()
	entries, _ := os.ReadDir(rundir.Dir())
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func body(t *testing.T, g *testenv.Gateway, n int) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(g.Body(n), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// holdSay makes the live habla process p hold say.pid.
func holdSay(t *testing.T, p *os.Process) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(rundir.Dir(), rundir.Say), []byte(strconv.Itoa(p.Pid)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestWithoutTTSSectionExits1(t *testing.T) {
	e, g, notes := setup(t, testenv.DefaultReplies, "")
	e.WriteConfigText(t, "[general]\nmax_seconds = 60\n")
	if code := Main([]string{"hola"}); code != 1 {
		t.Fatal(code)
	}
	want(t, notes(), "TTS is not configured: add a [tts.providers.<name>] section to config.toml")
	want(t, g.Paths())
}

func TestInvalidConfigExits1(t *testing.T) {
	e, g, _ := setup(t, testenv.DefaultReplies, "")
	path := filepath.Join(e.Dir, "config", "habla", "config.toml")
	os.WriteFile(path, []byte("[tts.providers.openai]\nvoice = 1\n"), 0o644)
	code := -1
	out := testenv.Stderr(t, func() { code = Main([]string{"hola"}) })
	if code != 1 || !strings.HasPrefix(out, "Invalid config: "+path+": toml: line 2") {
		t.Fatalf("exit %d, stderr %q", code, out)
	}
	want(t, g.Paths())
}

func TestPlaysWithPwPlayAndCleansUp(t *testing.T) {
	e, g, _ := setup(t, testenv.DefaultReplies, "")
	if code := Main([]string{"hola"}); code != 0 {
		t.Fatal(code)
	}
	want(t, e.Lines("played"), "pw-play "+filepath.Join(rundir.Dir(), "say.wav")+" RIFF-fake-speech")
	want(t, g.Paths(), "/v1/audio/speech")
	s := body(t, g, 0)
	if s["model"] != "gpt-4o-mini-tts" || s["voice"] != "alloy" || s["input"] != "hola" || s["response_format"] != "wav" || len(s) != 4 {
		t.Fatal(s)
	}
	want(t, runtimeFiles(t))
}

func TestFallsBackToPaplay(t *testing.T) {
	e, _, _ := setup(t, testenv.DefaultReplies, "")
	e.Remove("pw-play")
	if code := Main([]string{"hola"}); code != 0 {
		t.Fatal(code)
	}
	want(t, e.Lines("played"), "paplay "+filepath.Join(rundir.Dir(), "say.wav")+" RIFF-fake-speech")
}

func TestNoPlayer(t *testing.T) {
	e, _, notes := setup(t, testenv.DefaultReplies, "")
	e.Remove("pw-play")
	e.Remove("paplay")
	if code := Main([]string{"hola"}); code != 1 {
		t.Fatal(code)
	}
	want(t, notes(), "no player: install pipewire-bin (pw-play) or pulseaudio-utils (paplay)")
	want(t, runtimeFiles(t))
}

func TestPlayerFailureExits1(t *testing.T) {
	_, _, notes := setup(t, testenv.DefaultReplies, "")
	t.Setenv("HABLA_FAKE_FAIL", "pw-play")
	if code := Main([]string{"hola"}); code != 1 {
		t.Fatal(code)
	}
	want(t, notes(), "Playback failed: pw-play exited with status 3")
	want(t, runtimeFiles(t))
}

func TestTextFromStdin(t *testing.T) {
	_, g, _ := setup(t, testenv.DefaultReplies, "voice = \"Puck\"\n")
	Stdin = strings.NewReader("  hello\n")
	code := -1
	output := testenv.Stderr(t, func() { code = Main(nil) })
	if code != 0 || output != "" {
		t.Fatalf("exit %d, stderr %q", code, output)
	}
	want(t, g.Paths(), "/v1/audio/speech") // no rewrite call
	if s := body(t, g, 0); s["input"] != "hello" || s["voice"] != "Puck" {
		t.Fatal(s)
	}
}

func TestTerminalProgress(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(strconv.FormatBool(failed), func(t *testing.T) {
			r := testenv.DefaultReplies
			if failed {
				r.SpeechStatus = 400
			}
			setup(t, r, "")
			Stdin = strings.NewReader("hello\n")
			master, slave := testenv.Pty(t)
			saved := os.Stderr
			os.Stderr = slave
			t.Cleanup(func() { os.Stderr = saved; started = nil })
			started = func(int) { slave.WriteString("playing\n") }
			code := Main(nil)
			os.Stderr = saved
			slave.Close()
			output, _ := io.ReadAll(master) // closing a PTY slave ends reads with EIO
			want := "Processing...\r\nplaying\r\nDone\r\n"
			wantCode := 0
			if failed {
				want, wantCode = "Processing...\r\n", 1
			}
			if string(output) != want || code != wantCode {
				t.Fatalf("exit %d, output %q; want exit %d, output %q", code, output, wantCode, want)
			}
		})
	}
}

func TestSpeakDoesNotUseChatCompletions(t *testing.T) {
	r := testenv.DefaultReplies
	r.ChatStatus = 500
	_, g, notes := setup(t, r, "")
	if code := Main([]string{"hola"}); code != 0 {
		t.Fatal(code)
	}
	if s := body(t, g, 0); s["input"] != "hola" {
		t.Fatal(s)
	}
	want(t, g.Paths(), "/v1/audio/speech")
	want(t, notes())
}

func TestSelectedStyleUsesOneSpeechRequest(t *testing.T) {
	extra := "voice = \"Puck\"\ninstructions_mode = \"inline\"\nstyle = \"sevilla\"\n[styles.sevilla]\ninstructions = \"Speak Andalusian Spanish.\"\n"
	_, g, _ := setup(t, testenv.DefaultReplies, extra)
	if code := Main([]string{"hola"}); code != 0 {
		t.Fatal(code)
	}
	if paths := g.Paths(); !slices.Equal(paths, []string{"/v1/audio/speech"}) {
		t.Fatal(paths)
	}
	s := body(t, g, 0)
	if s["voice"] != "Puck" || !strings.HasPrefix(s["input"].(string), "<systemInstructions>") {
		t.Fatal(s)
	}
}

func TestStyleWithoutModeUsesPlainText(t *testing.T) {
	extra := "instructions_mode = \"\"\nstyle = \"sevilla\"\n[styles.sevilla]\ninstructions = \"Speak Andalusian Spanish.\"\n"
	_, g, _ := setup(t, testenv.DefaultReplies, extra)
	if code := Main([]string{"hola"}); code != 0 {
		t.Fatal(code)
	}
	s := body(t, g, 0)
	if s["voice"] != "alloy" || s["input"] != "hola" {
		t.Fatal(s)
	}
}

func TestOutWritesTheAudioWithoutPlaying(t *testing.T) {
	e, g, _ := setup(t, testenv.DefaultReplies, "")
	out := filepath.Join(e.Dir, "a.wav")
	if code := Main([]string{"--out", out, "hola"}); code != 0 {
		t.Fatal(code)
	}
	if data, _ := os.ReadFile(out); string(data) != "RIFF-fake-speech" {
		t.Fatalf("%q", data)
	}
	want(t, e.Lines("started")) // nothing played
	if s := body(t, g, 0); s["input"] != "hola" {
		t.Fatal(s)
	}
}

func TestEmptyTextSendsNothing(t *testing.T) {
	_, g, notes := setup(t, testenv.DefaultReplies, "")
	Stdin = strings.NewReader(" \n")
	if code := Main(nil); code != 1 {
		t.Fatal(code)
	}
	want(t, notes(), "Nothing to say")
	want(t, g.Paths())
}

func TestGatewayErrorExits1(t *testing.T) {
	r := testenv.DefaultReplies
	r.SpeechStatus = 400
	_, _, notes := setup(t, r, "")
	if code := Main([]string{"hola"}); code != 1 {
		t.Fatal(code)
	}
	want(t, notes(), "Gateway 400: voice not supported")
	want(t, runtimeFiles(t))
}

func TestStopEndsThePlayingProcess(t *testing.T) {
	setup(t, testenv.DefaultReplies, "")
	p := testenv.HablaProcess(t)
	holdSay(t, p.Process)
	if code := Main([]string{"--stop"}); code != 0 {
		t.Fatal(code)
	}
	err := p.Wait()
	if st, ok := p.ProcessState.Sys().(syscall.WaitStatus); !ok || !st.Signaled() || st.Signal() != syscall.SIGTERM {
		t.Fatal(err, p.ProcessState)
	}
}

func TestStopWithoutPlayback(t *testing.T) {
	_, _, notes := setup(t, testenv.DefaultReplies, "")
	if code := Main([]string{"--stop"}); code != 1 {
		t.Fatal(code)
	}
	want(t, notes(), "Not speaking")
}

func TestSIGTERMDuringPlaybackStopsThePlayer(t *testing.T) {
	e, _, _ := setup(t, testenv.DefaultReplies, "")
	t.Setenv("HABLA_FAKE_PLAY", "block")
	done := make(chan int, 1)
	go func() { done <- Main([]string{"hola"}) }()
	testenv.WaitFor(t, "the player", func() bool { return len(e.Lines("played")) > 0 })
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil { // what `habla say --stop` sends
		t.Fatal(err)
	}
	select {
	case code := <-done:
		if code != 0 {
			t.Fatal(code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("say did not stop")
	}
	want(t, e.Lines("player"), "terminated")
	want(t, runtimeFiles(t))
}

func TestSecondSayWhileSpeaking(t *testing.T) {
	_, g, notes := setup(t, testenv.DefaultReplies, "")
	p := testenv.HablaProcess(t)
	holdSay(t, p.Process)
	if code := Main([]string{"hola"}); code != 1 {
		t.Fatal(code)
	}
	want(t, notes(), "Already speaking: habla say --stop")
	want(t, g.Paths())
}

func TestFlagsMayFollowTheText(t *testing.T) {
	e, _, _ := setup(t, testenv.DefaultReplies, "")
	out := filepath.Join(e.Dir, "a.wav")
	if code := Main([]string{"hola", "--out", out}); code != 0 {
		t.Fatal(code)
	}
	if data, _ := os.ReadFile(out); string(data) != "RIFF-fake-speech" {
		t.Fatalf("%q", data)
	}
	if code := Main([]string{"uno", "dos"}); code != 2 {
		t.Fatal(code)
	}
}

// A stop arriving right after the player is spawned must still terminate and reap it.
func TestSayIsRecordedInStatsWithoutText(t *testing.T) {
	r := testenv.DefaultReplies
	r.Cost = "0.004"
	setup(t, r, "")
	if code := Main([]string{"--out", filepath.Join(t.TempDir(), "a.wav"), "hola mundo"}); code != 0 {
		t.Fatal(code)
	}
	records, err := stats.Load(stats.Path())
	if err != nil || len(records) != 1 {
		t.Fatal(records, err)
	}
	got := records[0]
	if got.Kind != "say" || got.Words != 2 || got.Chars != 10 || len(got.Calls) != 1 ||
		got.Calls[0].Service != "tts" || got.Calls[0].Cost != 0.004 || got.Calls[0].Source != "header" {
		t.Fatalf("%+v", got)
	}
	data, _ := os.ReadFile(stats.Path())
	if strings.Contains(string(data), "hola") {
		t.Fatal("stats.jsonl holds the spoken text")
	}
}

func TestFailedSayIsNotRecorded(t *testing.T) {
	r := testenv.DefaultReplies
	r.SpeechStatus = 400
	setup(t, r, "")
	if code := Main([]string{"hola"}); code != 1 {
		t.Fatal(code)
	}
	if _, err := os.Stat(stats.Path()); !os.IsNotExist(err) {
		t.Fatal("a failed say was recorded")
	}
}

func TestStopDuringPlayerStartupReapsThePlayer(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGINT} {
		t.Run(sig.String(), func(t *testing.T) {
			setup(t, testenv.DefaultReplies, "")
			t.Setenv("HABLA_FAKE_PLAY", "block")
			var pid int
			started = func(p int) { pid = p; syscall.Kill(os.Getpid(), sig) }
			defer func() { started = nil }()
			if code := Main([]string{"hola"}); code != 0 {
				t.Fatal(code)
			}
			if pid == 0 || syscall.Kill(pid, 0) == nil {
				t.Fatalf("player %d not reaped", pid)
			}
			want(t, runtimeFiles(t))
		})
	}
}

func TestProviderFlagPicksTheTTSProvider(t *testing.T) {
	e, g, _ := setup(t, testenv.DefaultReplies, "")
	e.WriteConfig(t, g, "[tts.providers.alt]\napi = \"openai\"\nendpoint = \""+g.URL+"\"\napi_key = \"env:HABLA_API_KEY\"\nmodel = \"alt-tts\"\nvoice = \"nova\"\n")
	out := filepath.Join(t.TempDir(), "a.wav")
	if code := Main([]string{"--provider", "alt", "--out", out, "hola"}); code != 0 {
		t.Fatal(code)
	}
	if m := body(t, g, 0); m["model"] != "alt-tts" || m["voice"] != "nova" {
		t.Fatalf("request: %v", m)
	}
	n := len(g.Paths())
	if code := Main([]string{"--provider", "nope", "--out", out, "hola"}); code != 1 || len(g.Paths()) != n {
		t.Fatalf("unknown provider: exit %d, %d requests", code, len(g.Paths())-n)
	}
}
