package live

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ackstorm/habla/internal/config"
	"github.com/ackstorm/habla/internal/testenv"
)

func TestMain(m *testing.M) {
	testenv.Fake()
	os.Exit(m.Run())
}

func TestContextFileValidation(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name, text string
		valid      bool
	}{
		{"notes.txt", "project codename: COBALT", true},
		{"empty.txt", "", true},
		{"invalid.txt", "private-context\xff", false},
		{"large.txt", strings.Repeat("x", (1<<20)+1), false},
	} {
		path := filepath.Join(dir, tc.name)
		if err := os.WriteFile(path, []byte(tc.text), 0600); err != nil {
			t.Fatal(err)
		}
		got, err := readContext(path)
		if (err == nil) != tc.valid || (tc.valid && got != tc.text) {
			t.Fatalf("%s: valid=%v, error=%v", tc.name, tc.valid, err)
		}
		if err != nil && strings.Contains(err.Error(), "private-context") {
			t.Fatal("context leaked in error")
		}
	}
	if _, err := readContext(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("missing context accepted")
	}
}

func TestCLIRejectsInvalidArgumentsAndMissingContext(t *testing.T) {
	testenv.Setup(t)
	for _, tc := range []struct {
		args []string
		code int
	}{
		{[]string{"--help"}, 0},
		{[]string{"--unknown"}, 2},
		{[]string{"unexpected"}, 2},
		{[]string{"--context", "missing.txt"}, 1},
		{[]string{"--no-output", "--output-file", "transcript.txt"}, 2},
	} {
		if got := Main(tc.args); got != tc.code {
			t.Fatalf("%v: got %d, want %d", tc.args, got, tc.code)
		}
	}
}

func TestTranscriptFileIsPrivateIncrementalAndNeverOverwritten(t *testing.T) {
	e := testenv.Setup(t)
	if err := os.MkdirAll(filepath.Dir(config.Path()), 0700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"", filepath.Join(e.Dir, "chosen.txt")} {
		f, err := openTranscript(path)
		if err != nil {
			t.Fatal(err)
		}
		if path == "" {
			name := filepath.Base(f.Name())
			if filepath.Dir(f.Name()) != filepath.Dir(config.Path()) || !strings.HasPrefix(name, "live_") || !strings.HasSuffix(name, ".txt") {
				t.Fatalf("unexpected default transcript name: %s", name)
			}
			if _, err := time.Parse("live_20060102_150405.txt", name); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := io.WriteString(f, "You: A question\nAssistant: A partial answer"); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(f.Name())
		if err != nil || string(data) != "You: A question\nAssistant: A partial answer" {
			t.Fatal("transcript was not saved incrementally")
		}
		info, err := f.Stat()
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("transcript is not private")
		}
		if _, err := openTranscript(f.Name()); err == nil {
			t.Fatal("existing transcript overwritten")
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCLITranscriptFlags(t *testing.T) {
	for _, mode := range []string{"default", "disabled", "custom"} {
		t.Run(mode, func(t *testing.T) {
			e := testenv.Setup(t)
			t.Setenv("HABLA_MISSING_LIVE_TOKEN", "")
			e.WriteConfigText(t, "[live.providers.test]\nendpoint = \"wss://unused.invalid/v1/realtime\"\nmodel = \"unused\"\napi_key = \"env:HABLA_MISSING_LIVE_TOKEN\"\n")
			_, terminal := testenv.Pty(t)
			stdin := os.Stdin
			os.Stdin = terminal
			defer func() { os.Stdin = stdin }()
			var args []string
			custom := filepath.Join(e.Dir, "discussion.txt")
			if mode == "disabled" {
				args = []string{"--no-output"}
			}
			if mode == "custom" {
				args = []string{"--output-file", custom}
			}
			// A missing credential ends startup without a network call.
			if code := Main(args); code != 1 {
				t.Fatalf("expected credential failure, got %d", code)
			}
			files, err := filepath.Glob(filepath.Join(filepath.Dir(config.Path()), "live_*.txt"))
			if err != nil {
				t.Fatal(err)
			}
			if (len(files) == 1) != (mode == "default") {
				t.Fatalf("unexpected default transcript files: %v", files)
			}
			_, err = os.Stat(custom)
			if (err == nil) != (mode == "custom") {
				t.Fatalf("custom transcript mismatch: %v", err)
			}
		})
	}
}

func TestAudioToolsStreamAndCleanup(t *testing.T) {
	for _, pulse := range []bool{false, true} {
		t.Run(map[bool]string{false: "pipewire", true: "pulse"}[pulse], func(t *testing.T) {
			e := testenv.Setup(t)
			if pulse {
				e.Remove("pw-record")
				e.Remove("pw-play")
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			recorder, err := audioCommand(true)
			if err != nil {
				t.Fatal(err)
			}
			cmd := audioProcess(ctx, recorder)
			mic, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { cancel(); mic.Close(); cmd.Wait() }()
			got := make([]byte, 960)
			if _, err := io.ReadFull(mic, got); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, testenv.HelloPCM(t)[:960]) {
				t.Fatal("recorder did not stream raw PCM")
			}
			player, err := audioCommand(false)
			if err != nil {
				t.Fatal(err)
			}
			play := audioProcess(ctx, player)
			r, w := io.Pipe()
			defer w.Close()
			defer r.Close()
			play.Stdin = r
			if err := play.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { cancel(); r.Close(); play.Wait() }()
			if _, err := w.Write(got); err != nil {
				t.Fatal(err)
			}
			testenv.WaitFor(t, "streaming player bytes", func() bool {
				info, err := os.Stat(filepath.Join(e.Dir, "raw-played"))
				return err == nil && info.Size() == 960
			})
			cancel()
			r.Close()
			mic.Close()
			done := make(chan struct{})
			go func() { cmd.Wait(); play.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("audio children did not exit")
			}
			if cmd.ProcessState == nil || play.ProcessState == nil {
				t.Fatal("audio children not reaped")
			}
		})
	}
}

func TestUnknownProviderFlagFails(t *testing.T) {
	e := testenv.Setup(t)
	e.WriteConfigText(t, "[live]\nprovider = \"a\"\n[live.providers.a]\nendpoint = \"wss://unused.invalid/v1/realtime\"\nmodel = \"unused\"\napi_key = \"env:HABLA_API_KEY\"\n[live.providers.b]\nendpoint = \"wss://unused.invalid/v1/realtime\"\nmodel = \"unused\"\napi_key = \"env:HABLA_API_KEY\"\n")
	stderr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	code := Main([]string{"--provider", "nope"})
	os.Stderr = stderr
	w.Close()
	out, _ := io.ReadAll(r)
	if code != 1 || !strings.Contains(string(out), "expected one of") {
		t.Fatalf("exit %d, stderr %q", code, out)
	}
}
