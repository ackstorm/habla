package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ackstorm/habla/internal/config"
)

// one returns the only recorded call, without its timing.
func one(t *testing.T, c *Client) Call {
	t.Helper()
	calls := c.Calls()
	if len(calls) != 1 {
		t.Fatalf("calls: %+v", calls)
	}
	calls[0].Ms = 0
	return calls[0]
}

func TestTranscriptionUsageIsRecorded(t *testing.T) {
	c, _ := serve(t, 200, `{"text":"hola","usage":{"type":"tokens","input_tokens":120,"input_token_details":{"audio_tokens":100,"text_tokens":20},"output_tokens":8}}`)
	if _, err := c.Transcribe(context.Background(), "m-stt", "", strings.NewReader("pcm"), 3); err != nil {
		t.Fatal(err)
	}
	if got := one(t, c); got != (Call{Service: "stt", Model: "m-stt", In: 20, Audio: 100, Out: 8}) {
		t.Fatalf("%+v", got)
	}
}

func TestGatewayCostHeaderIsKept(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-litellm-response-cost", "0.0012")
		io.WriteString(w, `{"choices":[{"message":{"content":"Hola."}}],"usage":{"prompt_tokens":50,"completion_tokens":5}}`)
	}))
	t.Cleanup(srv.Close)
	c := New(config.Conn{Endpoint: srv.URL + "/v1", API: "openai", AuthHeader: "Authorization", AuthScheme: "bearer"}, "sk")
	if _, err := c.Transform(context.Background(), "m-llm", "Fix.", "hola"); err != nil {
		t.Fatal(err)
	}
	if got := one(t, c); got != (Call{Service: "llm", Model: "m-llm", In: 50, Out: 5, Cost: 0.0012, Costed: true}) {
		t.Fatalf("%+v", got)
	}
}

func TestGeminiUsageMetadataIsRecorded(t *testing.T) {
	c, _ := serve(t, 200, `{"candidates":[{"content":{"parts":[{"audioTranscription":{"text":"hola"}}]}}],"usageMetadata":{"promptTokenCount":130,"candidatesTokenCount":4,"thoughtsTokenCount":6,"promptTokensDetails":[{"modality":"TEXT","tokenCount":30},{"modality":"AUDIO","tokenCount":100}]}}`)
	c.conn.API = "gemini"
	if _, err := c.Transcribe(context.Background(), "m-gem", "", strings.NewReader("pcm"), 3); err != nil {
		t.Fatal(err)
	}
	if got := one(t, c); got != (Call{Service: "stt", Model: "m-gem", In: 30, Audio: 100, Out: 10}) {
		t.Fatalf("%+v", got)
	}
}

func TestOnlySuccessfulRepliesAreRecorded(t *testing.T) {
	c, _ := serve(t, 500, `{"error":{"message":"down"}}`)
	if _, err := c.Transform(context.Background(), "m-llm", "Fix.", "hola"); err == nil {
		t.Fatal("want an error")
	}
	if calls := c.Calls(); len(calls) != 0 {
		t.Fatalf("a failed reply was recorded: %+v", calls)
	}
	s, _ := serve(t, 200, "RIFF-audio")
	if _, err := s.Speak(context.Background(), &config.TTS{Conn: config.Conn{Model: "m-tts"}, Format: "wav"}, "alloy", "", "hola"); err != nil {
		t.Fatal(err)
	}
	if got := one(t, s); got != (Call{Service: "tts", Model: "m-tts"}) { // audio bytes: no usage
		t.Fatalf("%+v", got)
	}
}
