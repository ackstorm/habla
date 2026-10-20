package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ackstorm/habla/internal/audio"
	"github.com/ackstorm/habla/internal/config"
)

var elevenPCM = []byte{1, 0, 2, 0, 3, 0}

func elevenClient(t *testing.T, status int) (*Client, *http.Request, *[]byte) {
	t.Helper()
	got, body := new(http.Request), new([]byte)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*got, *body = *r, nil
		*body, _ = io.ReadAll(r.Body)
		if status != 200 {
			w.WriteHeader(status)
			io.WriteString(w, `{"detail":{"message":"invalid api key"}}`)
			return
		}
		w.Header().Set("character-cost", "5")
		w.Write(elevenPCM)
	}))
	t.Cleanup(srv.Close)
	conn := config.Conn{API: "elevenlabs", Endpoint: srv.URL + "/v1", AuthHeader: "xi-api-key", AuthScheme: "raw", Model: "eleven_multilingual_v2"}
	return New(conn, "xi-secret-key"), got, body
}

func TestElevenLabsSpeak(t *testing.T) {
	c, got, body := elevenClient(t, 200)
	tts := &config.TTS{Conn: c.conn}
	data, err := c.Speak(context.Background(), tts, "JB/x", "ignored", "Hola.")
	if err != nil {
		t.Fatal(err)
	}
	if got.URL.EscapedPath() != "/v1/text-to-speech/JB%2Fx" || got.URL.RawQuery != "output_format=pcm_24000" {
		t.Fatal(got.URL)
	}
	if got.Header.Get("xi-api-key") != "xi-secret-key" || got.Header.Get("Authorization") != "" || got.Header.Get("Content-Type") != "application/json" {
		t.Fatal(got.Header)
	}
	var req map[string]string
	if json.Unmarshal(*body, &req) != nil || len(req) != 2 || req["text"] != "Hola." || req["model_id"] != "eleven_multilingual_v2" {
		t.Fatalf("%s", *body)
	}
	if !bytes.Equal(data, append(audio.HeaderAt(24000, int64(len(elevenPCM))), elevenPCM...)) {
		t.Fatalf("% x", data)
	}
	if call := one(t, c); call != (Call{Service: "tts", Model: "eleven_multilingual_v2", In: 5}) {
		t.Fatalf("%+v", call)
	}
}

func TestElevenLabsErrorNamesStatusNotKey(t *testing.T) {
	c, _, _ := elevenClient(t, 401)
	_, err := c.Speak(context.Background(), &config.TTS{Conn: c.conn}, "v", "", "Hola.")
	if err == nil || !strings.Contains(err.Error(), "401") || strings.Contains(err.Error(), "xi-secret-key") {
		t.Fatal(err)
	}
	if len(c.Calls()) != 0 {
		t.Fatal("a failed reply is recorded")
	}
}

func TestElevenLabsErrorShowsDetailMessage(t *testing.T) {
	for _, body := range []string{`{"detail":{"status":"invalid_api_key","message":"Invalid API key"}}`, `{"detail":"Invalid API key"}`} {
		if got := errorMessage(401, []byte(body)); got != "Invalid API key" {
			t.Errorf("%s: %q", body, got)
		}
	}
}

func TestCharacterCostOnlyCountsForElevenLabs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("character-cost", "99")
		io.WriteString(w, `{"choices":[{"message":{"content":"Hola."}}],"usage":{"prompt_tokens":50,"completion_tokens":5}}`)
	}))
	t.Cleanup(srv.Close)
	c := New(config.Conn{Endpoint: srv.URL + "/v1", API: "openai", AuthHeader: "Authorization", AuthScheme: "bearer"}, "sk")
	if _, err := c.Transform(context.Background(), "m", "Fix.", "hola"); err != nil {
		t.Fatal(err)
	}
	if got := one(t, c); got.In != 50 {
		t.Fatalf("%+v", got)
	}
}

func TestHeaderAtSetsRate(t *testing.T) {
	h := audio.HeaderAt(24000, 4)
	if h[24] != 0xC0 || h[25] != 0x5D || h[28] != 0x80 || h[29] != 0xBB { // 24000 = 0x5DC0, 48000 = 0xBB80
		t.Fatalf("% x", h)
	}
}
