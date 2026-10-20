package live

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ackstorm/habla/internal/config"
	"github.com/gorilla/websocket"
)

func socket(t *testing.T, serve func(*websocket.Conn)) string {
	return socketSession(t, map[string]any{"modalities": []string{"audio"}}, serve)
}

func socketSession(t *testing.T, session map[string]any, serve func(*websocket.Conn)) string {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" || r.URL.Query().Get("model") != "test-model" {
			http.Error(w, "invalid authentication or model", 400)
			return
		}
		c, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		c.SetReadDeadline(time.Now().Add(5 * time.Second))
		c.WriteJSON(map[string]any{"type": "session.created", "session": session})
		serve(c)
	}))
	t.Cleanup(s.Close)
	return "ws" + strings.TrimPrefix(s.URL, "http") + "/v1/realtime"
}

func TestLiveStyleIsSentThroughTheSelectedMode(t *testing.T) {
	for _, mode := range []string{"", "inline", "instructions"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			problem := make(chan string, 2)
			url := socketSession(t, map[string]any{"type": "realtime"}, func(c *websocket.Conn) {
				var e struct {
					Type    string `json:"type"`
					Session struct {
						Type         string `json:"type"`
						Instructions string `json:"instructions"`
						Audio        struct {
							Output struct {
								Voice string `json:"voice"`
							} `json:"output"`
						} `json:"audio"`
					} `json:"session"`
				}
				if c.ReadJSON(&e) != nil || e.Type != "session.update" || e.Session.Type != "realtime" {
					problem <- "not a GA session update"
					return
				}
				if mode == "instructions" {
					if e.Session.Instructions != "Speak with an Andalusian accent." || e.Session.Audio.Output.Voice != "marin" {
						problem <- "native style missing"
						return
					}
					c.WriteJSON(map[string]any{"type": "session.updated", "session": map[string]any{"instructions": "Speak with an Andalusian accent.", "audio": map[string]any{"output": map[string]any{"voice": "marin"}}}})
				} else if e.Session.Instructions != "" || e.Session.Audio.Output.Voice != "" {
					problem <- "native style applied outside native mode"
					return
				}
				var item struct {
					Item struct {
						Content []struct {
							Text string `json:"text"`
						} `json:"content"`
					} `json:"item"`
				}
				if c.ReadJSON(&item) != nil || len(item.Item.Content) != 1 {
					problem <- "context missing"
					return
				}
				text := item.Item.Content[0].Text
				if !strings.Contains(text, "project context") || (strings.Contains(text, "Andalusian") != (mode == "inline")) {
					problem <- "inline style/context mismatch"
					return
				}
				// inline: the style in its own block; always ends with the muted-reply wording
				inlineBlock := strings.HasPrefix(text, "<systemInstructions>\n") && strings.Contains(text, "Speak with an Andalusian accent.\n</systemInstructions>")
				if inlineBlock != (mode == "inline") || strings.Contains(text, "Reply to this message only with") {
					problem <- "opening message: " + text
					return
				}
				problem <- ""
				// GA: nothing else may follow the item (the client's only later writes would be mic audio, none here)
				c.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
				if _, m, err := c.ReadMessage(); err == nil {
					problem <- "unexpected message after the item: " + string(m)
				} else {
					problem <- ""
				}
			})
			c, opening, err := connect(ctx, config.Live{Endpoint: url, Model: "test-model", InstructionsMode: mode}, "test-token", "project context", config.Delivery{Voice: "marin", Instructions: "Speak with an Andalusian accent."})
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			if message := <-problem; message != "" {
				t.Fatal(message)
			}
			if opening {
				t.Fatal("GA sessions must not expect an opening reply")
			}
			if message := <-problem; message != "" {
				t.Fatal(message)
			}
		})
	}
}

func TestNativeStyleCannotBeSilentlyIgnored(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	url := socket(t, func(c *websocket.Conn) {
		var update any
		c.ReadJSON(&update)
		c.WriteJSON(map[string]any{"type": "session.updated", "session": map[string]any{}})
		c.ReadMessage()
	})
	_, _, err := connect(ctx, config.Live{Endpoint: url, Model: "test-model", InstructionsMode: "instructions"}, "test-token", "", config.Delivery{Instructions: "private-style"})
	if err == nil || strings.Contains(err.Error(), "private-style") {
		t.Fatalf("ignored or leaked native style: %v", err)
	}
}

func TestContextAndMicrophoneStreamBeforeResponseCompletes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	played := make(chan []byte, 1)
	serverErr := make(chan string, 1)
	url := socket(t, func(c *websocket.Conn) {
		var setup, background map[string]any
		if c.ReadJSON(&setup) != nil || setup["type"] != "session.update" {
			serverErr <- "no session setup"
			return
		}
		if c.ReadJSON(&background) != nil || background["type"] != "conversation.item.create" {
			serverErr <- "no context item"
			return
		}
		encoded, _ := json.Marshal(background)
		if !bytes.Contains(encoded, []byte("project codename: COBALT")) || !bytes.Contains(encoded, []byte("Reply to this message only with: OK.")) {
			serverErr <- "context or its muted-reply wording missing"
			return
		}
		c.WriteJSON(map[string]any{"type": "response.created"})
		c.WriteJSON(map[string]any{"type": "response.output_audio_transcript.delta", "delta": "OK"})
		c.WriteJSON(map[string]any{"type": "response.output_audio.delta", "delta": base64.StdEncoding.EncodeToString([]byte{9, 9})})
		c.WriteJSON(map[string]any{"type": "response.done", "response": map[string]any{"status": "completed"}})
		var input struct {
			Type  string `json:"type"`
			Audio []byte `json:"audio"`
		}
		if c.ReadJSON(&input) != nil || input.Type != "input_audio_buffer.append" || !bytes.Equal(input.Audio, bytes.Repeat([]byte{1, 2}, 480)) {
			serverErr <- "microphone PCM missing or changed"
			return
		}
		c.WriteJSON(map[string]any{"type": "response.created"})
		c.WriteJSON(map[string]any{"type": "response.output_audio.delta", "delta": base64.StdEncoding.EncodeToString([]byte{3, 4, 5, 6})})
		select {
		case pcm := <-played:
			if !bytes.Equal(pcm, []byte{3, 4, 5, 6}) {
				serverErr <- "wrong playback PCM"
				return
			}
		case <-ctx.Done():
			serverErr <- "playback waited for response.done"
			return
		}
		c.WriteJSON(map[string]any{"type": "conversation.item.input_audio_transcription.completed", "transcript": "Hello world"})
		c.WriteJSON(map[string]any{"type": "response.output_audio_transcript.delta", "delta": "COBALT"})
		c.WriteJSON(map[string]any{"type": "response.done", "response": map[string]any{"status": "completed"}})
		var input2 struct {
			Type  string `json:"type"`
			Audio []byte `json:"audio"`
		}
		if c.ReadJSON(&input2) != nil || input2.Type != "input_audio_buffer.append" {
			serverErr <- "microphone did not resume"
			return
		}
		serverErr <- ""
		cancel()
		c.ReadMessage()
	})
	c, opening, err := connect(ctx, config.Live{Endpoint: url, Model: "test-model"}, "test-token", "project codename: COBALT", config.Delivery{})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	mic, feed := io.Pipe()
	defer feed.Close()
	go func() {
		for {
			if _, err := feed.Write(bytes.Repeat([]byte{1, 2}, 480)); err != nil {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
	}()
	play := func(ctx context.Context, r io.Reader) error {
		data := make([]byte, 4)
		if _, err := io.ReadFull(r, data); err != nil {
			return err
		}
		played <- data
		_, err := io.Copy(io.Discard, r)
		return err
	}
	var transcript bytes.Buffer
	file, err := openTranscript(filepath.Join(t.TempDir(), "conversation.txt"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	err = converse(ctx, c, mic, play, opening, io.MultiWriter(&transcript, file), io.Discard)
	if problem := <-serverErr; problem != "" {
		t.Fatal(problem)
	}
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(transcript.String(), "OK") {
		t.Fatalf("muted opening reply was written: %q", transcript.String())
	}
	if !strings.Contains(transcript.String(), "You: Hello world") || !strings.Contains(transcript.String(), "Assistant: COBALT") {
		t.Fatalf("missing transcript: %q", transcript.String())
	}
	saved, err := os.ReadFile(file.Name())
	if err != nil || !bytes.Equal(saved, transcript.Bytes()) || bytes.Contains(saved, []byte("project codename:")) {
		t.Fatal("saved transcript differs from conversation output or includes raw context")
	}
}

type failedTranscript struct{}

func (failedTranscript) Write([]byte) (int, error) {
	return 0, errors.New("private-context token-value")
}

func TestTranscriptWriteFailureStopsSessionWithoutLeakingContent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	url := socket(t, func(c *websocket.Conn) {
		var setup any
		c.ReadJSON(&setup)
		c.WriteJSON(map[string]any{"type": "conversation.item.input_audio_transcription.completed", "transcript": "private-context"})
		c.ReadMessage()
	})
	c, _, err := connect(ctx, config.Live{Endpoint: url, Model: "test-model"}, "test-token", "", config.Delivery{})
	if err != nil {
		t.Fatal(err)
	}
	mic, feed := io.Pipe()
	defer feed.Close()
	err = converse(ctx, c, mic, func(context.Context, io.Reader) error { return nil }, false, failedTranscript{}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "transcript") || strings.Contains(err.Error(), "private-context") || strings.Contains(err.Error(), "token-value") {
		t.Fatalf("unsafe or missing transcript error: %v", err)
	}
}

func TestProtocolFailuresDoNotEchoPayloads(t *testing.T) {
	for _, payload := range []string{
		`{"type":"error","error":{"message":"private-context test-token"}}`,
		`{"type":"response.output_audio.delta","delta":"private-context test-token"}`,
		`{"type":"response.done","response":{"status":"failed","status_details":{"message":"private-context test-token"}}}`,
		`invalid private-context test-token`,
	} {
		t.Run(payload[:7], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			url := socket(t, func(c *websocket.Conn) {
				var setup any
				c.ReadJSON(&setup)
				c.WriteMessage(websocket.TextMessage, []byte(payload))
				c.ReadMessage()
			})
			c, _, err := connect(ctx, config.Live{Endpoint: url, Model: "test-model"}, "test-token", "", config.Delivery{})
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			mic, feed := io.Pipe()
			defer feed.Close()
			err = converse(ctx, c, mic, func(context.Context, io.Reader) error { return nil }, false, io.Discard, io.Discard)
			if err == nil || strings.Contains(err.Error(), "private-context") || strings.Contains(err.Error(), "test-token") {
				t.Fatalf("unsafe or missing error: %v", err)
			}
		})
	}
}

func TestCancelUnblocksCaptureAndPlayback(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	url := socket(t, func(c *websocket.Conn) {
		var setup any
		c.ReadJSON(&setup)
		c.WriteJSON(map[string]any{"type": "response.created"})
		c.WriteJSON(map[string]any{"type": "response.output_audio.delta", "delta": "AQIDBA=="})
		c.ReadMessage()
	})
	c, _, err := connect(ctx, config.Live{Endpoint: url, Model: "test-model"}, "test-token", "", config.Delivery{})
	if err != nil {
		t.Fatal(err)
	}
	mic, feed := io.Pipe()
	defer feed.Close()
	playing := make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		finished <- converse(ctx, c, mic, func(ctx context.Context, r io.Reader) error {
			close(playing)
			<-ctx.Done()
			return ctx.Err()
		}, false, io.Discard, io.Discard)
	}()
	select {
	case <-playing:
	case <-ctx.Done():
		t.Fatal("playback did not start")
	}
	cancel()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation left a blocked worker")
	}
}

func TestMicrophoneResumesOnlyAfterPlaybackDrains(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	draining := make(chan struct{})
	release := make(chan struct{})
	checked := make(chan string, 1)
	url := socket(t, func(c *websocket.Conn) {
		var setup any
		c.ReadJSON(&setup)
		c.WriteJSON(map[string]any{"type": "response.created"})
		c.WriteJSON(map[string]any{"type": "response.output_audio.delta", "delta": "AQIDBA=="})
		c.WriteJSON(map[string]any{"type": "response.done", "response": map[string]any{"status": "completed"}})
		<-draining
		c.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		_, _, err := c.ReadMessage()
		if err == nil {
			checked <- "microphone forwarded during playback drain"
		} else {
			checked <- ""
		}
		close(release)
		<-ctx.Done()
	})
	c, _, err := connect(ctx, config.Live{Endpoint: url, Model: "test-model"}, "test-token", "", config.Delivery{})
	if err != nil {
		t.Fatal(err)
	}
	mic, feed := io.Pipe()
	defer feed.Close()
	go func() {
		<-draining
		for {
			if _, err := feed.Write(make([]byte, 960)); err != nil {
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	err = converse(ctx, c, mic, func(ctx context.Context, r io.Reader) error {
		if _, err := io.Copy(io.Discard, r); err != nil {
			return err
		}
		close(draining)
		select {
		case <-release:
			cancel()
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}, false, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if problem := <-checked; problem != "" {
		t.Fatal(problem)
	}
}

func TestAudioBurstWaitsForPlayback(t *testing.T) {
	for _, abort := range []bool{false, true} {
		t.Run(map[bool]string{false: "drain", true: "cancel"}[abort], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			// 12.5 seconds of PCM arrives at once, exceeding the ten-second queue.
			pcm := bytes.Repeat([]byte{1, 2}, 300000)
			url := socket(t, func(c *websocket.Conn) {
				var setup any
				c.ReadJSON(&setup)
				c.WriteJSON(map[string]any{"type": "response.created"})
				c.WriteJSON(map[string]any{"type": "response.output_audio.delta", "delta": base64.StdEncoding.EncodeToString(pcm)})
				c.WriteJSON(map[string]any{"type": "response.done", "response": map[string]any{"status": "completed"}})
				c.ReadMessage()
			})
			c, _, err := connect(ctx, config.Live{Endpoint: url, Model: "test-model"}, "test-token", "", config.Delivery{})
			if err != nil {
				t.Fatal(err)
			}
			mic, feed := io.Pipe()
			defer feed.Close()
			playing, release := make(chan struct{}), make(chan struct{})
			played := make(chan []byte, 1)
			finished := make(chan error, 1)
			go func() {
				finished <- converse(ctx, c, mic, func(ctx context.Context, r io.Reader) error {
					close(playing)
					select {
					case <-release:
					case <-ctx.Done():
						return ctx.Err()
					}
					data, err := io.ReadAll(r)
					played <- data
					return err
				}, false, io.Discard, io.Discard)
			}()
			select {
			case <-playing:
			case <-ctx.Done():
				t.Fatal("playback did not start")
			}
			select {
			case err := <-finished:
				t.Fatalf("ordinary audio burst aborted the session: %v", err)
			case <-time.After(100 * time.Millisecond):
			}
			if !abort {
				close(release)
				select {
				case data := <-played:
					if !bytes.Equal(data, pcm) {
						t.Fatalf("playback lost audio: got %d bytes, want %d", len(data), len(pcm))
					}
				case <-ctx.Done():
					t.Fatal("buffered audio did not drain")
				}
			}
			cancel()
			select {
			case err := <-finished:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("cancellation left the full queue blocked")
			}
		})
	}
}

func TestHandshakeErrorOmitsResponseBody(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "private-context test-token", http.StatusUnauthorized)
	}))
	defer s.Close()
	_, _, err := connect(context.Background(), config.Live{Endpoint: "ws" + strings.TrimPrefix(s.URL, "http"), Model: "test-model"}, "test-token", "", config.Delivery{})
	if err == nil || !strings.Contains(err.Error(), "401") || strings.Contains(err.Error(), "test-token") || strings.Contains(err.Error(), "private-context") {
		t.Fatalf("unsafe handshake error: %v", err)
	}
}
