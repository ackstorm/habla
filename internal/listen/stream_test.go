package listen

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ackstorm/habla/internal/audio"
	"github.com/ackstorm/habla/internal/config"
	"github.com/ackstorm/habla/internal/gateway"
	"github.com/ackstorm/habla/internal/rundir"
	"github.com/ackstorm/habla/internal/testenv"
)

func started(t *testing.T, g *testenv.Gateway) {
	t.Helper()
	select {
	case <-g.Started:
	case <-time.After(10 * time.Second):
		t.Fatal("nothing was uploaded while recording")
	}
}

func TestUploadDoesNotWaitForTransformCredential(t *testing.T) {
	for _, tc := range []struct {
		name, key string
		stream    bool
	}{
		{"stream", "sk-test", true},
		{"file", "sk-test", false},
		{"stream with failed credential", "", true},
		{"file with failed credential", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, g, _ := setup(t, testenv.DefaultReplies, conf)
			fifo := filepath.Join(e.Dir, "llm-key")
			if err := syscall.Mkfifo(fifo, 0o600); err != nil {
				t.Fatal(err)
			}
			gate, err := os.OpenFile(fifo, os.O_RDWR, 0)
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("HABLA_TEST_KEY_PIPE", fifo)
			cfg := load(t)
			cfg.LLM.APIKey = `cmd:read key < "$HABLA_TEST_KEY_PIPE"; printf '%s' "$key"`
			wav := recorded(t, "current.wav")
			u := startUpload(cfg, wav)
			d := &dictation{cfg: cfg}
			t.Cleanup(func() {
				gate.WriteString("\n") // release the credential command even when an assertion fails
				<-u.ready
				<-d.parts[0].done
				gate.Close()
			})
			if tc.stream {
				u.streamWhileRecording()
				t.Cleanup(u.abort)
			}
			d.add(wav, u)
			select {
			case <-g.Started:
			case <-time.After(3 * time.Second):
				t.Fatal("upload waited for the transform credential")
			}
			if _, err := gate.WriteString(tc.key + "\n"); err != nil {
				t.Fatal(err)
			}
			if got := result(t, finishing(d)); got != (finished{0, false}) {
				t.Fatal(got)
			}
			if tc.key == "" {
				want(t, e.Lines("copied"), "Eh, pues hola.")
				want(t, g.Paths(), "/v1/audio/transcriptions")
			} else {
				want(t, e.Lines("copied"), "Hola.")
				want(t, g.Paths(), "/v1/audio/transcriptions", "/v1/chat/completions")
			}
		})
	}
}

func TestUploadIntervalWaitsWhileRecordingAndFlushesOnStop(t *testing.T) {
	_, g, _ := setup(t, testenv.DefaultReplies, conf+"[general]\nupload_interval_ms = 1000\n")
	wav := recorded(t, "current.wav")
	u := startUpload(load(t), wav)
	<-u.ready
	u.streamWhileRecording()
	t.Cleanup(u.abort)
	select {
	case <-g.Started:
		t.Fatal("upload started before the configured polling interval")
	case <-time.After(750 * time.Millisecond):
	}
	started(t, g)
	done := make(chan transcript, 1)
	go func() { text, err := u.finish(wav); done <- transcript{text: text, err: err} }()
	select {
	case r := <-done:
		if r.err != nil || r.text != "Eh, pues hola." {
			t.Fatalf("transcription: %q, %v", r.text, r.err)
		}
	case <-time.After(750 * time.Millisecond):
		t.Fatal("stopping waited for the next polling interval")
	}
}

func TestSpeechIsUploadedWhileRecording(t *testing.T) {
	e, g, _ := setup(t, testenv.DefaultReplies, conf)
	keys, wait := interactive(t)
	recording(t)
	started(t, g) // before Enter: the upload began with the speech
	testenv.WaitFor(t, "the whole recording", func() bool { return len(e.Lines("recorded")) > 0 })
	keys("\n")
	if code := wait(); code != 0 {
		t.Fatal(code)
	}
	want(t, e.Lines("copied"), "Hola.")
	want(t, g.Paths(), "/v1/audio/transcriptions", "/v1/chat/completions")
	// the hello clip trims to [0, 53120): its trimmed end lies past len-holdBack (48000), so the bytes are exact.
	pcm := testenv.HelloPCM(t)
	start, end := audio.Trim(pcm, -45)
	if !bytes.Equal(g.Audio()[0], append(audio.Header(-1), pcm[start:end]...)) {
		t.Fatal("the streamed upload is not the trimmed recording behind the streaming header")
	}
	emptyRunDir(t)
}

func TestGeminiSpeechIsUploadedWhileRecording(t *testing.T) {
	e, g, _ := setup(t, testenv.DefaultReplies, conf+"[stt.providers.openai]\napi = \"gemini\"\nmodel = \"gemini-3.5-transcribe\"\n")
	keys, wait := interactive(t)
	recording(t)
	started(t, g)
	testenv.WaitFor(t, "the whole recording", func() bool { return len(e.Lines("recorded")) > 0 })
	keys("\n")
	if code := wait(); code != 0 {
		t.Fatal(code)
	}
	want(t, e.Lines("copied"), "Hola.")
	want(t, g.Paths(), "/v1/models/gemini-3.5-transcribe:generateContent", "/v1/chat/completions")
	pcm := testenv.HelloPCM(t)
	start, end := audio.Trim(pcm, -45)
	if !bytes.Equal(g.Audio()[0], append(audio.Header(-1), pcm[start:end]...)) {
		t.Fatal("the streamed upload is not the trimmed recording behind the streaming header")
	}
}

func TestFailedStreamGetsOneNormalUpload(t *testing.T) {
	r := testenv.DefaultReplies
	r.STTStatus = 500
	e, g, notes := setup(t, r, conf)
	keys, wait := interactive(t)
	recording(t)
	started(t, g)
	keys("\n")
	if code := wait(); code != 1 {
		t.Fatal(code)
	}
	want(t, g.Paths(), "/v1/audio/transcriptions", "/v1/audio/transcriptions")
	if a := g.Audio(); len(a) != 2 || !bytes.Equal(a[0][40:44], []byte{0xff, 0xff, 0xff, 0xff}) || bytes.Equal(a[1][40:44], []byte{0xff, 0xff, 0xff, 0xff}) {
		t.Fatal("want one streamed upload, then one with exact sizes")
	}
	want(t, notes(), note{"Gateway 500: no access", RetryHint, false})
	if !exists("failed.wav") {
		t.Fatal("failed.wav missing")
	}
	want(t, e.Lines("copied"))
}

func TestEscAbortsTheUpload(t *testing.T) {
	e, g, _ := setup(t, testenv.DefaultReplies, conf)
	keys, wait := interactive(t)
	recording(t)
	started(t, g)
	keys("\x1b")
	if code := wait(); code != 1 {
		t.Fatal(code)
	}
	// The body can never finish (the last half second is held back), so the gateway sees the request end early
	// only if Habla aborted it; no transcription is ever completed.
	select {
	case <-g.Aborted:
	case <-time.After(10 * time.Second):
		t.Fatal("the upload was not aborted")
	}
	if n := len(g.Audio()); n != 0 {
		t.Fatalf("%d transcriptions completed", n)
	}
	want(t, e.Lines("copied"))
	for _, p := range g.Paths() {
		if strings.HasSuffix(p, "/chat/completions") {
			t.Fatal("a discarded recording was transformed")
		}
	}
	emptyRunDir(t)
}

func TestNoSpeechSendsNothing(t *testing.T) {
	e, g, notes := setup(t, testenv.DefaultReplies, conf+"trim_below_db = -1\n") // no frame is that loud
	keys, wait := interactive(t)
	recording(t)
	testenv.WaitFor(t, "the whole recording", func() bool { return len(e.Lines("recorded")) > 0 })
	keys("\n")
	if code := wait(); code != 1 {
		t.Fatal(code)
	}
	want(t, g.Paths())
	want(t, notes(), note{"Nothing heard.", "", false})
}

// A current.wav left by a killed session must not leak into the next upload.
func TestStaleRecordingIsNotUploaded(t *testing.T) {
	e, g, _ := setup(t, testenv.DefaultReplies, conf)
	loud := bytes.Repeat([]byte{0x00, 0x40}, 32000) // 2 s of a loud tone-less signal, final-size header
	if err := os.MkdirAll(rundir.Dir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file("current.wav"), append(audio.Header(int64(len(loud))), loud...), 0o600); err != nil {
		t.Fatal(err)
	}
	// A recorder that is slow to start gives the upload time to read the stale file first.
	exe, _ := os.Executable()
	e.Remove("pw-record")
	script := "#!/bin/bash\n/bin/sleep 0.4\nexec -a pw-record " + exe + ` "$@"` + "\n"
	if err := os.WriteFile(filepath.Join(e.Bin, "pw-record"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	keys, wait := interactive(t)
	recording(t)
	testenv.WaitFor(t, "the whole recording", func() bool { return len(e.Lines("recorded")) > 0 })
	keys("\n")
	if code := wait(); code != 0 {
		t.Fatal(code)
	}
	pcm := testenv.HelloPCM(t)
	start, end := audio.Trim(pcm, -45)
	if a := g.Audio(); len(a) != 1 || !bytes.Equal(a[0], append(audio.Header(-1), pcm[start:end]...)) {
		t.Fatal("the upload is not exactly the new recording")
	}
}

func shortTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	saved := gateway.TranscribeTimeout
	gateway.TranscribeTimeout = d
	t.Cleanup(func() { gateway.TranscribeTimeout = saved })
}

// A stream whose Write is stalled never delivers a result: finish must abort it after TranscribeTimeout and fall
// back to a normal upload.
func TestFinishAbortsAStalledStream(t *testing.T) {
	setup(t, testenv.DefaultReplies, conf)
	shortTimeout(t, 200*time.Millisecond)
	cfg := load(t)
	wav := recorded(t, "current.wav")
	u := startUpload(cfg, wav)
	u.result = make(chan transcript, 1)
	u.cancel = func() { u.result <- transcript{err: context.Canceled, streamed: true} }
	done := make(chan string, 1)
	go func() { text, _ := u.finish(wav); done <- text }()
	select {
	case text := <-done:
		if text != "Eh, pues hola." {
			t.Fatal(text)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("finish is stuck behind the stalled stream")
	}
}

// A gateway that takes the connection and never answers ends in a kept failed.wav, not a hang.
func TestUnresponsiveGatewayKeepsTheRecording(t *testing.T) {
	e, _, notes := setup(t, testenv.DefaultReplies, conf)
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body) // after the body the server notices a closed connection
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	e.WriteConfig(t, &testenv.Gateway{URL: srv.URL + "/v1"}, conf)
	shortTimeout(t, 300*time.Millisecond)
	done := make(chan int, 1)
	go func() { done <- runProcess(t) }()
	select {
	case code := <-done:
		if code != 1 {
			t.Fatal(code)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the session hangs on an unresponsive gateway")
	}
	if !exists("failed.wav") || rundir.ReadState() != "failed" || len(notes()) != 1 {
		t.Fatal("want failed.wav, state failed and one notification", notes())
	}
}

// speechStart must begin where audio.Trim does, or the stream and the file fallback would send different audio.
func TestSpeechStartMatchesTrim(t *testing.T) {
	tone := testenv.HelloPCM(t)
	pcm := append(make([]byte, 32000), tone...) // 1 s of silence, then speech
	for _, db := range []int{-60, -45, -30, -20} {
		begin, _ := speechStart(pcm, 0, db)
		start, _ := audio.Trim(pcm, db)
		if begin != start {
			t.Errorf("%d dB: speechStart %d, Trim %d", db, begin, start)
		}
	}
}

// twoPhrases is the hello clip, 1 s of silence and the hello clip again: hello's voice ends at 1.46 s, so the pause is 1.54 s.
func twoPhrases(t *testing.T) []byte {
	hello := testenv.HelloPCM(t)
	return append(append(slices.Clone(hello), make([]byte, 32000)...), hello...)
}

func splitConfig(after, pause float64, belowDB int) *config.Config {
	return &config.Config{General: config.General{TrimBelowDB: belowDB}, Split: config.Split{Enabled: true, After: after, Pause: pause}}
}

func TestSplitterCutsAtALongPause(t *testing.T) {
	pcm := twoPhrases(t)
	_, end := audio.Trim(testenv.HelloPCM(t), -45)
	s := newSplitter(splitConfig(1, 1, -45))
	begin, voiced := speechStart(pcm, 0, -45)
	s.start(voiced)
	if cut := s.cut(pcm[:40000]); cut != -1 { // still in the first phrase
		t.Fatalf("cut %d before the pause", cut)
	}
	if cut := s.cut(pcm); cut != end {
		t.Fatalf("cut %d, want %d (where Trim ends the first phrase)", cut, end)
	}
	next, _ := speechStart(pcm, s.scanned, -45)
	_, voiced = speechStart(pcm[96000:], 0, -45)
	if want := 96000 + (voiced-audio.PadFrames)*audio.FrameBytes; next != want || next <= begin {
		t.Fatalf("the second piece starts at %d, want %d (its first voiced frame minus the padding)", next, want)
	}
}

func TestSplitterKeepsOnePiece(t *testing.T) {
	pcm := twoPhrases(t)
	for name, s := range map[string]*splitter{
		"pause too short": newSplitter(splitConfig(1, 2, -45)),
		"piece too short": newSplitter(splitConfig(4, 1, -45)),
		"trim off":        newSplitter(splitConfig(1, 1, 0)),
		"split off":       newSplitter(&config.Config{General: config.General{TrimBelowDB: -45}, Split: config.Split{After: 1, Pause: 1}}),
	} {
		_, voiced := speechStart(pcm, 0, -45)
		s.start(voiced)
		if cut := s.cut(pcm); cut != -1 {
			t.Errorf("%s: cut at %d", name, cut)
		}
	}
}

func TestJoinedPieces(t *testing.T) {
	piece := func(text string, err error) chan transcript {
		c := make(chan transcript, 1)
		c <- transcript{text: text, err: err, streamed: true}
		return c
	}
	if got := joined([]chan transcript{piece(" Uno. ", nil), piece("", nil), piece("Dos.", nil)}); got != (transcript{text: "Uno. Dos.", streamed: true}) {
		t.Fatal(got)
	}
	failed := errors.New("down")
	if got := joined([]chan transcript{piece("Uno.", nil), piece("", failed)}); got.err != failed {
		t.Fatal(got)
	}
	if got := joined(nil); got != (transcript{}) {
		t.Fatal(got)
	}
}

func TestLongPauseSplitsTheUpload(t *testing.T) {
	e, g, _ := setup(t, testenv.DefaultReplies, conf+"[split]\nenabled = true\nafter = 1\npause = 1\n")
	wav := filepath.Join(e.Dir, "two.wav")
	pcm := twoPhrases(t)
	if err := os.WriteFile(wav, append(audio.Header(int64(len(pcm))), pcm...), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HABLA_FAKE_RECORDING", wav)
	keys, wait := interactive(t)
	recording(t)
	started(t, g)
	started(t, g) // the second piece, before Enter
	testenv.WaitFor(t, "the whole recording", func() bool { return len(e.Lines("recorded")) > 0 })
	keys("\n")
	if code := wait(); code != 0 {
		t.Fatal(code)
	}
	want(t, g.Paths(), "/v1/audio/transcriptions", "/v1/audio/transcriptions", "/v1/chat/completions")
	for _, a := range g.Audio() {
		if !bytes.Equal(a[40:44], []byte{0xff, 0xff, 0xff, 0xff}) {
			t.Fatal("want two streamed pieces")
		}
	}
	if body := string(g.Body(2)); !strings.Contains(body, "Eh, pues hola. Eh, pues hola.") {
		t.Fatal(body)
	}
	want(t, e.Lines("copied"), "Hola.")
	emptyRunDir(t)
}

func TestSplitterThresholds(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		after, pause          float64
		start, voice, silence int // 20 ms frames
		cut                   bool
	}{
		{"minimum not reached", 10, 1, 0, 400, 60, false},
		{"new segment restarts minimum", 10, 1, 600, 400, 60, false},
		{"pause too short", 10, 1, 0, 500, 49, false},
		{"one second pause", 10, 1, 0, 500, 50, true},
		{"fractional pause too short", 10, 0.75, 0, 500, 37, false},
		{"fractional pause reached", 10, 0.75, 0, 500, 38, true},
		{"unreachable minimum", 1e300, 1, 0, 500, 50, false},
		{"unreachable pause", 10, 1e300, 0, 500, 50, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pcm := make([]byte, tc.start*audio.FrameBytes)
			pcm = append(pcm, bytes.Repeat([]byte{0, 16}, tc.voice*audio.FrameBytes/2)...)
			pcm = append(pcm, make([]byte, tc.silence*audio.FrameBytes)...)
			s := newSplitter(splitConfig(tc.after, tc.pause, -45))
			s.start(0)
			s.start(tc.start)
			want := -1
			if tc.cut {
				want = (tc.start + tc.voice + 10) * audio.FrameBytes
			} // 200 ms padding
			if got := s.cut(pcm); got != want {
				t.Fatalf("cut = %d, want %d", got, want)
			}
		})
	}
}

func TestDefaultSplitUpload(t *testing.T) {
	for _, tc := range []struct {
		name, extra string
		pieces      int
	}{
		{"defaults", "", 2},
		{"explicit opt out", "[split]\nenabled = false\n", 1},
		{"trim disabled", "[general]\ntrim_below_db = 0\n", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, g, _ := setup(t, testenv.DefaultReplies, testenv.Merge(conf, tc.extra))
			hello := testenv.HelloPCM(t)
			pcm := bytes.Repeat(hello, 5)
			pcm = append(pcm, make([]byte, 2*audio.Rate*66/100)...)
			pcm = append(pcm, hello...) // 10 s, a 1.2 s pause (including hello's tail), then 2 s
			wav := filepath.Join(e.Dir, "default-split.wav")
			if err := os.WriteFile(wav, append(audio.Header(int64(len(pcm))), pcm...), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("HABLA_FAKE_RECORDING", wav)
			keys, wait := interactive(t)
			finished := false
			t.Cleanup(func() {
				if !finished {
					keys("\x1b")
					wait()
				}
			})
			recording(t)
			for range tc.pieces {
				started(t, g)
			}
			testenv.WaitFor(t, "the whole recording", func() bool { return len(e.Lines("recorded")) > 0 })
			keys("\n")
			code := wait()
			finished = true
			if code != 0 {
				t.Fatal(code)
			}
			paths := make([]string, tc.pieces)
			for i := range paths {
				paths[i] = "/v1/audio/transcriptions"
			}
			want(t, g.Paths(), append(paths, "/v1/chat/completions")...)
			for _, a := range g.Audio() {
				if !bytes.Equal(a[40:44], []byte{0xff, 0xff, 0xff, 0xff}) {
					t.Fatal("want streamed pieces")
				}
			}
			text := strings.TrimSpace(strings.Repeat("Eh, pues hola. ", tc.pieces))
			if body := string(g.Body(tc.pieces)); !strings.Contains(body, text) {
				t.Fatal(body)
			}
			want(t, e.Lines("copied"), "Hola.")
			emptyRunDir(t)
		})
	}
}
