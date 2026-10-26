package live

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ackstorm/habla/internal/audio"
	"github.com/ackstorm/habla/internal/config"
	"github.com/ackstorm/habla/internal/gateway"
)

// This exercises recorded audio and context, not a physical microphone or audible playback.
func TestRealtimeGatewayRecordedAudio(t *testing.T) {
	env := map[string]string{}
	var missing []string
	for _, k := range []string{"HABLA_LIVE_REALTIME_URL", "HABLA_LIVE_REALTIME_MODEL", "HABLA_LIVE_TTS_URL", "HABLA_LIVE_TTS_MODEL", "HABLA_API_KEY"} {
		if env[k] = os.Getenv(k); env[k] == "" {
			missing = append(missing, k)
		}
	}
	if os.Getenv("HABLA_LIVE") != "1" || len(missing) > 0 {
		t.Skip("set HABLA_LIVE=1 and " + strings.Join(missing, ", ") + " to call the realtime gateway")
	}
	key := env["HABLA_API_KEY"]
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	c, opening, err := connect(ctx, config.Live{Endpoint: env["HABLA_LIVE_REALTIME_URL"], Model: env["HABLA_LIVE_REALTIME_MODEL"], InstructionsMode: "inline"}, key,
		"The project codename is COBALT. When I say hello world, reply with only the project codename.", config.Delivery{Instructions: "Keep every answer short and speak with a calm delivery."}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{}, 1)
	// The bridge takes 24 kHz PCM: synthesize "Hello world." with the same gateway's TTS (a 24 kHz WAV).
	tts := &config.TTS{Conn: config.Conn{API: "openai", Endpoint: env["HABLA_LIVE_TTS_URL"], AuthHeader: "Authorization", AuthScheme: "bearer", Model: env["HABLA_LIVE_TTS_MODEL"]}, Voice: "Kore", Format: "wav"}
	speech, err := gateway.New(tts.Conn, key).Speak(ctx, tts, tts.Voice, "", "Hello world.")
	if err != nil {
		t.Fatal(err)
	}
	offset, length, ok := audio.Data(speech)
	if !ok {
		t.Fatal("speech WAV has no data chunk")
	}
	pcm := speech[offset : offset+length]
	pcm = append(pcm, make([]byte, 48000)...)
	mic, feed := io.Pipe()
	defer feed.Close()
	go func() {
		for range 2 {
			select {
			case <-ready:
			case <-ctx.Done():
				return
			}
			time.Sleep(50 * time.Millisecond)
			for pos := 0; pos < len(pcm); pos += 960 {
				if _, err := feed.Write(pcm[pos:min(pos+960, len(pcm))]); err != nil {
					return
				}
				time.Sleep(20 * time.Millisecond)
			}
		}
		for {
			if _, err := feed.Write(make([]byte, 960)); err != nil {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	var turns atomic.Int32
	var audioBytes atomic.Int64
	var transcript bytes.Buffer
	err = converse(ctx, c, mic, func(ctx context.Context, r io.Reader) error {
		prefix := make([]byte, 12)
		np, err := io.ReadFull(r, prefix)
		if err != nil {
			return err
		}
		if bytes.HasPrefix(prefix, []byte("RIFF")) || bytes.HasPrefix(prefix, []byte("OggS")) {
			return errors.New("expected raw PCM, received an audio container")
		}
		n, err := io.Copy(io.Discard, r)
		n += int64(np)
		audioBytes.Add(n)
		if turns.Add(1) >= 2 {
			cancel()
		}
		return err
	}, opening, &transcript, listening(ready), nil)
	if err != nil {
		t.Fatal(err)
	}
	if turns.Load() < 2 || audioBytes.Load() == 0 || strings.Count(strings.ToLower(transcript.String()), "cobalt") < 2 || strings.Count(strings.ToLower(transcript.String()), "hello") < 2 {
		t.Fatal("recorded speech/context exchange did not complete (payload omitted)")
	}
	t.Logf("two recorded-audio turns completed; %d output PCM bytes", audioBytes.Load())
}

// listening signals ready whenever converse reports that the microphone is open again.
type listening chan struct{}

func (l listening) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte("Listening")) {
		select {
		case l <- struct{}{}:
		default:
		}
	}
	return len(p), nil
}

// The gateway passes tools to Gemini only with LiteLLM's gemini_live_defer_setup (enabled 2026-10-26): without it the
// model says it has no tools. Text in, so no TTS is needed.
func TestRealtimeGatewayToolCall(t *testing.T) {
	url, model, key := os.Getenv("HABLA_LIVE_REALTIME_URL"), os.Getenv("HABLA_LIVE_REALTIME_MODEL"), os.Getenv("HABLA_API_KEY")
	if os.Getenv("HABLA_LIVE") != "1" || url == "" || model == "" || key == "" {
		t.Skip("set HABLA_LIVE=1, HABLA_LIVE_REALTIME_URL, HABLA_LIVE_REALTIME_MODEL and HABLA_API_KEY to call the realtime gateway")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	dir := t.TempDir()
	files, err := openTools(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer files.root.Close()
	c, _, err := connect(ctx, config.Live{Endpoint: url, Model: model, InstructionsMode: "inline"}, key, "", config.Delivery{}, files)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.WriteJSON(map[string]any{"type": "conversation.item.create", "item": map[string]any{"type": "message", "role": "user",
		"content": []map[string]string{{"type": "input_text", "text": "Create the file hello.txt containing exactly: hola"}}}})
	c.WriteJSON(map[string]string{"type": "response.create"})
	called, followed := false, false
	for ctx.Err() == nil {
		c.SetReadDeadline(time.Now().Add(30 * time.Second))
		var e event
		if err := c.ReadJSON(&e); err != nil {
			t.Fatal(err)
		}
		switch {
		case e.Type == "error":
			t.Fatal("the gateway sent an error")
		case e.Type == "response.output_item.done" && e.Item.Type == "function_call":
			output, summary := files.call(e.Item.Name, e.Item.Arguments)
			t.Logf("Tool: %s", summary)
			c.WriteJSON(map[string]any{"type": "conversation.item.create", "item": map[string]any{
				"type": "function_call_output", "call_id": e.Item.CallID, "output": output}})
			called = true
		case e.Type == "response.done" && e.Response.Status != "" && e.Response.Status != "completed":
			t.Fatalf("response.done status %q: converse would end the session", e.Response.Status)
		case e.Type == "response.done" && called && !followed: // as converse does
			c.WriteJSON(map[string]string{"type": "response.create"})
			followed = true
		case e.Type == "response.done" && followed:
			if data, err := os.ReadFile(filepath.Join(dir, "hello.txt")); err != nil || strings.TrimSpace(string(data)) != "hola" {
				t.Fatalf("hello.txt = %q, %v", data, err)
			}
			// A bridge that also answered the tool output by itself would overlap converse's follow-up.
			c.SetReadDeadline(time.Now().Add(3 * time.Second))
			for c.ReadJSON(&e) == nil {
				if e.Type == "response.created" {
					t.Fatal("a second response answered the tool output")
				}
			}
			return
		case e.Type == "response.done":
			t.Fatal("the response ended without a tool call: are tools reaching the model?")
		}
	}
	t.Fatal(ctx.Err())
}
