package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ackstorm/habla/internal/audio"
	"github.com/ackstorm/habla/internal/config"
)

type request struct {
	method, path, auth, contentType, remote string
	body                                    []byte
}

func serve(t *testing.T, status int, reply string) (*Client, func() []request) {
	t.Helper()
	var mu sync.Mutex
	var seen []request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen = append(seen, request{r.Method, r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("Content-Type"), r.RemoteAddr, body})
		mu.Unlock()
		w.WriteHeader(status)
		io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)
	return New(config.Conn{Endpoint: srv.URL + "/v1", API: "openai", AuthHeader: "Authorization", AuthScheme: "bearer"}, "sk-env"), func() []request { mu.Lock(); defer mu.Unlock(); return append([]request{}, seen...) }
}

func TestCredentialSources(t *testing.T) {
	t.Setenv("HABLA_TEST_KEY", " sk-env\n")
	for _, tc := range []struct{ source, key, err string }{
		{"env:HABLA_TEST_KEY", "sk-env", ""},
		{"env:HABLA_UNSET_KEY", "", "No API key: $HABLA_UNSET_KEY is not set."},
		{"cmd:printf sk-cmd", "sk-cmd", ""},
		{"cmd:false", "", "No API key: `false` failed."},
		{"cmd:printf 'two words'", "", "No API key: `printf 'two words'` did not provide a single token."},
		{"cmd:(sleep 6 &); printf sk-x", "sk-x", ""},
	} {
		key, err := Credential(context.Background(), tc.source)
		if key != tc.key || (err == nil) != (tc.err == "") || (err != nil && err.Error() != tc.err) {
			t.Errorf("%s: %q %v", tc.source, key, err)
		}
	}
}

func TestCredentialCommandHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	key, err := Credential(ctx, "cmd:exec sleep 30")
	if err == nil || key != "" || time.Since(started) > time.Second {
		t.Fatalf("credential command did not stop promptly: %v", err)
	}
}

func TestAuthHeaderAndScheme(t *testing.T) {
	for _, tc := range []struct{ header, scheme, wantHeader, want string }{
		{"Authorization", "bearer", "Authorization", "Bearer sk-1"}, {"x-api-key", "raw", "X-Api-Key", "sk-1"},
	} {
		var got http.Header
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got = r.Header.Clone()
			io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
		}))
		c := New(config.Conn{Endpoint: srv.URL + "/v1", API: "openai", AuthHeader: tc.header, AuthScheme: tc.scheme}, "sk-1")
		_, err := c.Transform(context.Background(), "m", "p", "t")
		srv.Close()
		if err != nil || got.Get(tc.wantHeader) != tc.want || (tc.header != "Authorization" && got.Get("Authorization") != "") {
			t.Errorf("%s/%s: %v %v", tc.header, tc.scheme, got, err)
		}
	}
}

func TestClientsOnOneHostShareATransport(t *testing.T) {
	a := New(config.Conn{Endpoint: "https://h.example/v1"}, "k")
	b := New(config.Conn{Endpoint: "https://h.example/gemini/v1beta"}, "k")
	c := New(config.Conn{Endpoint: "https://other.example/v1"}, "k")
	if a.http.Transport != b.http.Transport || a.http.Transport == c.http.Transport {
		t.Fatal("transport sharing")
	}
}

func TestRedirectsStayOnConfiguredOrigin(t *testing.T) {
	for _, header := range []string{"Authorization", "x-goog-api-key", "xi-api-key"} {
		for _, status := range []int{http.StatusFound, http.StatusSeeOther} {
			for _, destination := range []string{"same-origin", "other-port", "other-host"} {
				t.Run(fmt.Sprintf("%s/%d/%s", header, status, destination), func(t *testing.T) {
					received := make(chan string, 1)
					target := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						received <- r.Header.Get(header)
						io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
					})
					other := httptest.NewServer(target)
					defer other.Close()
					location := other.URL
					if destination == "same-origin" {
						location = "/redirected"
					} else if destination == "other-host" {
						location = strings.Replace(other.URL, "127.0.0.1", "localhost", 1)
					}
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.URL.Path == "/redirected" {
							target.ServeHTTP(w, r)
							return
						}
						http.Redirect(w, r, location, status)
					}))
					defer srv.Close()
					c := New(config.Conn{Endpoint: srv.URL, AuthHeader: header, AuthScheme: "raw"}, "test-key")
					_, err := c.Transform(context.Background(), "m", "p", "x")
					if destination == "same-origin" {
						if err != nil || len(received) != 1 || <-received != "test-key" {
							t.Fatalf("same-origin redirect lost authentication: %v", err)
						}
					} else if err == nil || len(received) != 0 {
						t.Fatalf("cross-origin redirect was followed: %v", err)
					}
				})
			}
		}
	}
}

func TestRedirectLoopStops(t *testing.T) {
	requests := make(chan struct{}, 11)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- struct{}{}
		http.Redirect(w, r, "/loop", http.StatusFound)
	}))
	defer srv.Close()
	c := New(config.Conn{Endpoint: srv.URL, AuthHeader: "Authorization"}, "test-key")
	_, err := c.Transform(context.Background(), "m", "p", "x")
	if err == nil || !strings.Contains(err.Error(), "stopped after 10 redirects") || len(requests) != 10 {
		t.Fatalf("redirect loop: %d requests, error %v", len(requests), err)
	}
}

func TestTranscribeBuildsMultipartWithLanguage(t *testing.T) {
	c, seen := serve(t, 200, `{"text":"Hola."}`)
	pcm := []byte{1, 2, 3, 4}
	if text, err := c.Transcribe(context.Background(), "gpt-4o-mini-transcribe", "es", bytes.NewReader(pcm), 4); text != "Hola." || err != nil {
		t.Fatal(text, err)
	}
	r := seen()[0]
	_, params, err := mime.ParseMediaType(r.contentType)
	if err != nil {
		t.Fatal(err)
	}
	mr := multipart.NewReader(bytes.NewReader(r.body), params["boundary"])
	model, _ := mr.NextPart()
	if model.FormName() != "model" || readPart(model) != "gpt-4o-mini-transcribe" {
		t.Fatal("model")
	}
	lang, _ := mr.NextPart()
	if lang.FormName() != "language" || readPart(lang) != "es" {
		t.Fatal("language")
	}
	file, _ := mr.NextPart()
	body, _ := io.ReadAll(file)
	if !bytes.Equal(body, append(audio.Header(4), pcm...)) {
		t.Fatal("file WAV")
	}
}

func TestStreamingTranscriptionUsesUnknownLengthWAVHeader(t *testing.T) {
	c, seen := serve(t, 200, `{"text":"ok"}`)
	r, w := io.Pipe()
	go func() {
		_, _ = w.Write([]byte{1, 2})
		time.Sleep(10 * time.Millisecond)
		_, _ = w.Write([]byte{3, 4})
		_ = w.Close()
	}()
	if text, err := c.Transcribe(context.Background(), "m", "", r, -1); err != nil || text != "ok" {
		t.Fatal(text, err)
	}
	req := seen()[0]
	_, params, _ := mime.ParseMediaType(req.contentType)
	mr := multipart.NewReader(bytes.NewReader(req.body), params["boundary"])
	_, _ = mr.NextPart()
	file, _ := mr.NextPart()
	body, _ := io.ReadAll(file)
	if !bytes.Equal(body, append(audio.Header(-1), 1, 2, 3, 4)) {
		t.Fatalf("streamed WAV: %v", body[:min(48, len(body))])
	}
}

func TestCredentialRejectsInvalidTokenWithoutEchoingIt(t *testing.T) {
	t.Setenv("HABLA_BAD_KEY", "sk SECRET")
	_, err := Credential(context.Background(), "env:HABLA_BAD_KEY")
	if err == nil || strings.Contains(err.Error(), "SECRET") || !strings.Contains(err.Error(), "did not provide a single token") {
		t.Fatal(err)
	}
}

func readPart(p *multipart.Part) string { b, _ := io.ReadAll(p); return string(b) }

func TestTransformRequestAndReply(t *testing.T) {
	c, seen := serve(t, 200, `{"choices":[{"message":{"content":" Hola. "}}]}`)
	out, err := c.Transform(context.Background(), "m", "Fix.", "eh hola")
	if err != nil || out != "Hola." {
		t.Fatal(out, err)
	}
	var got map[string]any
	json.Unmarshal(seen()[0].body, &got)
	want := map[string]any{"model": "m", "temperature": 0.0, "messages": []any{
		map[string]any{"role": "system", "content": "Fix.\n\n" + TransformSuffix},
		map[string]any{"role": "user", "content": "<transcript>eh hola</transcript>"},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatal(got)
	}
}

func TestTransformRejectsMissingText(t *testing.T) {
	for _, reply := range []string{`{"choices":[]}`, `{"choices":[{"message":{"content":null}}]}`, `{"choices":[{"message":{"content":""}}]}`, `{"choices":[{"message":{"content":" \n\t "}}]}`} {
		c, _ := serve(t, 200, reply)
		if _, err := c.Transform(context.Background(), "m", "p", "x"); err == nil || err.Error() != "Gateway reply has no transform text" {
			t.Fatalf("%s: %v", reply, err)
		}
	}
}

func TestSpeakSendsOriginalText(t *testing.T) {
	c, seen := serve(t, 200, "RIFF-speech")
	tts := &config.TTS{Conn: config.Conn{Model: "gpt-4o-mini-tts"}, Voice: "alloy", Format: "wav"}
	got, err := c.Speak(context.Background(), tts, "alloy", "", "Hola.")
	if err != nil || string(got) != "RIFF-speech" {
		t.Fatal(string(got), err)
	}
	r := seen()[0]
	var body map[string]any
	json.Unmarshal(r.body, &body)
	want := map[string]any{"model": "gpt-4o-mini-tts", "voice": "alloy", "input": "Hola.", "response_format": "wav"}
	if !reflect.DeepEqual(body, want) || r.path != "/v1/audio/speech" {
		t.Fatal(r, body)
	}
}

func TestSpeakRequestBodies(t *testing.T) {
	for _, tc := range []struct {
		mode, instructions string
		wantInput          string
		wantInstructions   string
	}{
		{"", "", "Hola.", ""},
		{"inline", "", "Hola.", ""},
		{"inline", "Andaluz.", fmt.Sprintf(InlineTemplate, "Andaluz.", "Hola."), ""},
		{"instructions", "Andaluz.", "Hola.", "Andaluz."},
	} {
		c, seen := serve(t, 200, "RIFF")
		tts := &config.TTS{Conn: config.Conn{Model: "gpt-4o-mini-tts"}, Format: "wav", InstructionsMode: tc.mode}
		if _, err := c.Speak(context.Background(), tts, "Puck", tc.instructions, "Hola."); err != nil {
			t.Fatal(err)
		}
		var got map[string]any
		if err := json.Unmarshal(seen()[0].body, &got); err != nil {
			t.Fatal(err)
		}
		want := map[string]any{"model": "gpt-4o-mini-tts", "voice": "Puck", "input": tc.wantInput, "response_format": "wav"}
		if tc.wantInstructions != "" {
			want["instructions"] = tc.wantInstructions
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: %v, want %v", tc.mode, got, want)
		}
	}
}

func TestGatewayErrorAndEmptySpeech(t *testing.T) {
	c, _ := serve(t, 403, `{"error":{"message":"no access"}}`)
	if _, err := c.Transcribe(context.Background(), "m", "", bytes.NewReader(nil), 0); err == nil || !strings.Contains(err.Error(), "Gateway 403: no access") {
		t.Fatal(err)
	}
	c, _ = serve(t, 200, "")
	if _, err := c.Speak(context.Background(), &config.TTS{Conn: config.Conn{Model: "m"}}, "", "", "x"); err == nil || err.Error() != "Gateway reply has no audio" {
		t.Fatal(err)
	}
}

func TestSpeakErrorIncludesGatewayMessage(t *testing.T) {
	c, _ := serve(t, 400, `{"error":{"message":"voice unsupported"}}`)
	if _, err := c.Speak(context.Background(), &config.TTS{Conn: config.Conn{Model: "m"}}, "", "", "x"); err == nil || err.Error() != "Gateway 400: voice unsupported" {
		t.Fatal(err)
	}
}

func TestTranscribeTimeoutIsReported(t *testing.T) {
	saved := TranscribeTimeout
	TranscribeTimeout = 30 * time.Millisecond
	t.Cleanup(func() { TranscribeTimeout = saved })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	c := New(config.Conn{Endpoint: srv.URL + "/v1", AuthHeader: "Authorization", AuthScheme: "bearer"}, "k")
	if _, err := c.Transcribe(context.Background(), "m", "", bytes.NewReader(nil), 0); err == nil || !strings.Contains(err.Error(), "no reply within 30ms") {
		t.Fatal(err)
	}
}

func TestOneConnectionServesSeveralCalls(t *testing.T) {
	c, seen := serve(t, 200, `{"text":"x","choices":[{"message":{"content":"y"}}]}`)
	c.Transcribe(context.Background(), "m", "", bytes.NewReader(nil), 0)
	c.Transform(context.Background(), "m", "p", "x")
	if r := seen(); r[0].remote != r[1].remote {
		t.Fatal("connection was not reused")
	}
}

func TestCredentialCommandDoesNotWaitForBackgroundChild(t *testing.T) {
	start := time.Now()
	key, err := Credential(context.Background(), "cmd:(sleep 6 &); printf sk-x")
	if err != nil || key != "sk-x" {
		t.Fatal(key, err)
	}
	if time.Since(start) > 4*time.Second {
		t.Fatal("waited for background process")
	}
}
