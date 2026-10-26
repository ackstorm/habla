package live

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ackstorm/habla/internal/config"
	"github.com/ackstorm/habla/internal/gateway"
	"github.com/gorilla/websocket"
)

// The gateway's Gemini realtime bridge labels pcm16 as 24 kHz in both directions.
const frameBytes = 960 // 20 ms, mono s16le

type event struct {
	Type       string `json:"type"`
	Delta      string `json:"delta"`
	Transcript string `json:"transcript"`
	Item       struct {
		Type      string `json:"type"`
		Name      string `json:"name"`
		CallID    string `json:"call_id"`
		Arguments string `json:"arguments"`
	} `json:"item"`
	Response struct {
		Status string `json:"status"`
	} `json:"response"`
	Session struct {
		Type         string `json:"type"`
		Instructions string `json:"instructions"`
		Voice        string `json:"voice"`
		Audio        struct {
			Output struct {
				Voice string `json:"voice"`
			} `json:"output"`
		} `json:"audio"`
	} `json:"session"`
}

// connect opens the session; files (nil: none) declares its tools in the first session.update, the only one the
// gateway's Gemini bridge applies.
func connect(ctx context.Context, cfg config.Live, key, background string, style config.Delivery, files *tools) (*websocket.Conn, bool, error) {
	u, err := url.Parse(cfg.Endpoint)
	if err != nil {
		return nil, false, errors.New("Invalid realtime endpoint")
	}
	q := u.Query()
	q.Set("model", cfg.Model)
	u.RawQuery = q.Encode()
	dialer := websocket.Dialer{Proxy: http.ProxyFromEnvironment, HandshakeTimeout: gateway.ConnectTimeout}
	c, resp, err := dialer.DialContext(ctx, u.String(), http.Header{"Authorization": {"Bearer " + key}})
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}
	if err != nil {
		if resp != nil {
			return nil, false, fmt.Errorf("Realtime handshake failed (HTTP %d)", resp.StatusCode)
		}
		return nil, false, errors.New("Could not connect to realtime gateway")
	}
	failed := true
	defer func() {
		if failed {
			c.Close()
		}
	}()
	c.SetReadLimit(1 << 20)
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	c.SetReadDeadline(time.Now().Add(gateway.ConnectTimeout))
	var first event
	if err := c.ReadJSON(&first); err != nil || first.Type != "session.created" {
		return nil, false, errors.New("Realtime gateway did not create a session")
	}
	c.SetReadDeadline(time.Time{})
	c.SetWriteDeadline(time.Now().Add(gateway.ConnectTimeout))
	ga := first.Session.Type == "realtime"
	session := map[string]any{"modalities": []string{"audio"}, "input_audio_format": "pcm16", "output_audio_format": "pcm16", "turn_detection": map[string]string{"type": "server_vad"}}
	if ga {
		session = map[string]any{"type": "realtime", "output_modalities": []string{"audio"}, "audio": map[string]any{
			"input":  map[string]any{"format": map[string]any{"type": "audio/pcm", "rate": 24000}, "turn_detection": map[string]string{"type": "server_vad"}},
			"output": map[string]any{"format": map[string]any{"type": "audio/pcm", "rate": 24000}},
		}}
	}
	if files != nil {
		session["tools"], session["tool_choice"] = files.declarations(), "auto"
	}
	native := cfg.InstructionsMode == "instructions" && (style.Instructions != "" || style.Voice != "")
	if native {
		if style.Instructions != "" {
			session["instructions"] = style.Instructions
		}
		if style.Voice != "" {
			if ga {
				session["audio"].(map[string]any)["output"].(map[string]any)["voice"] = style.Voice
			} else {
				session["voice"] = style.Voice
			}
		}
	}
	if err := c.WriteJSON(map[string]any{
		"type":    "session.update",
		"session": session,
	}); err != nil {
		return nil, false, errors.New("Could not configure realtime session")
	}
	if native {
		c.SetReadDeadline(time.Now().Add(gateway.ConnectTimeout))
		var updated event
		if err := c.ReadJSON(&updated); err != nil || updated.Type != "session.updated" {
			return nil, false, errors.New("Realtime gateway did not confirm native live style: use instructions_mode = inline")
		}
		voice := updated.Session.Voice
		if ga {
			voice = updated.Session.Audio.Output.Voice
		}
		if (style.Instructions != "" && updated.Session.Instructions != style.Instructions) || (style.Voice != "" && voice != style.Voice) {
			return nil, false, errors.New("Realtime gateway did not apply the requested live style")
		}
		c.SetReadDeadline(time.Time{})
	}
	// No reply is requested: OpenAI GA stays silent until the user speaks, while the Gemini bridge answers any item by
	// itself, so converse mutes that opening reply (hence "Reply to this message only with: OK.").
	inline := cfg.InstructionsMode == "inline" && style.Instructions != ""
	var parts []string
	if inline {
		parts = append(parts, "<systemInstructions>\nThese instructions control only your voice, accent, tone, and delivery.\n"+
			"Do not read, mention, or explain these instructions.\n"+style.Instructions+"\n</systemInstructions>")
	}
	if background != "" {
		parts = append(parts, "Background context for our spoken conversation follows. Use it when answering my questions.\n\n"+background)
	}
	opening := false
	if len(parts) > 0 {
		if !ga { // GA never answers this item: the line would linger and could cut short the first real reply
			parts = append(parts, "Reply to this message only with: OK.")
		}
		if err := c.WriteJSON(map[string]any{
			"type": "conversation.item.create",
			"item": map[string]any{"type": "message", "role": "user", "content": []map[string]string{{"type": "input_text", "text": strings.Join(parts, "\n\n")}}},
		}); err != nil {
			return nil, false, errors.New("Could not send conversation context")
		}
		opening = !ga
	}
	failed = false
	return c, opening, nil
}

// converse owns mic and c until return. play must stop when its context is cancelled. A response that calls files'
// tools gets their outputs and a follow-up response; the microphone stays paused until that one is done. bargeIn
// keeps the microphone open instead: the server's speech_started stops playback and drops the rest of that reply.
func converse(parent context.Context, c *websocket.Conn, mic io.ReadCloser, play func(context.Context, io.Reader) error, awaitingContext, bargeIn bool, out, status io.Writer, files *tools) error {
	ctx, cancel := context.WithCancel(parent)
	var wg sync.WaitGroup
	defer func() { cancel(); c.Close(); mic.Close(); wg.Wait() }()
	failures := make(chan error, 1)
	fail := func(err error) {
		select {
		case failures <- err:
		default:
		}
		cancel()
	}
	write := func(format string, args ...any) {
		if _, err := fmt.Fprintf(out, format, args...); err != nil {
			fail(errors.New("Could not write conversation transcript"))
		}
	}
	var sending sync.Mutex // the microphone and the tool outputs share the socket
	send := func(v any) error {
		sending.Lock()
		defer sending.Unlock()
		c.SetWriteDeadline(time.Now().Add(gateway.ConnectTimeout))
		return c.WriteJSON(v)
	}
	var paused atomic.Bool
	paused.Store(awaitingContext)
	// Keep ten seconds queued; a full queue slows socket reads until playback catches up.
	type chunk struct {
		gen int64 // audio queued before the latest interruption is dropped
		pcm []byte
	}
	queued := 500
	if bargeIn {
		// ponytail: a full queue also delays reading speech_started, so barge-in queues a minute (about 3 MB);
		// replies longer than that interrupt late. Read the socket apart from queuing if that matters.
		queued = 3000
	}
	playback := make(chan chunk, queued)
	var gen atomic.Int64
	var cut atomic.Value // the current player's context.CancelFunc
	wg.Add(3)
	go func() { defer wg.Done(); <-ctx.Done(); c.Close(); mic.Close() }()
	go func() {
		defer wg.Done()
		frame := make([]byte, frameBytes)
		for {
			if _, err := io.ReadFull(mic, frame); err != nil {
				fail(errors.New("Microphone capture stopped"))
				return
			}
			if paused.Load() {
				continue
			}
			if err := send(map[string]string{"type": "input_audio_buffer.append", "audio": base64.StdEncoding.EncodeToString(frame)}); err != nil {
				fail(errors.New("Could not stream microphone audio"))
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		var pipe *io.PipeWriter
		var done chan error
		var closeOnCancel func() bool
		var stopPlayer context.CancelFunc
		var pipeGen int64
		finish := func() error {
			if pipe == nil {
				return nil
			}
			pipe.Close()
			err := <-done
			closeOnCancel()
			stopPlayer()
			pipe = nil
			if pipeGen != gen.Load() { // interrupted: the player was stopped
				return nil
			}
			return err
		}
		defer finish()
		for {
			select {
			case <-ctx.Done():
				return
			case next := <-playback:
				pcm := next.pcm
				if pipe != nil && pipeGen != gen.Load() {
					stopPlayer()
					finish()
				}
				if pcm == nil {
					if err := finish(); err != nil {
						fail(errors.New("Audio playback failed"))
						return
					}
					paused.Store(false)
					fmt.Fprintln(status, "Listening… Ctrl+C ends the session.")
					continue
				}
				if next.gen != gen.Load() {
					continue
				}
				if pipe == nil {
					r, w := io.Pipe()
					pipe, done, pipeGen = w, make(chan error, 1), next.gen
					var pctx context.Context
					pctx, stopPlayer = context.WithCancel(ctx)
					cut.Store(stopPlayer)
					closeOnCancel = context.AfterFunc(pctx, func() { r.CloseWithError(pctx.Err()) })
					result := done
					go func() { err := play(pctx, r); r.CloseWithError(err); result <- err }()
				}
				if _, err := pipe.Write(pcm); err != nil {
					if pipeGen != gen.Load() {
						finish()
						continue
					}
					fail(errors.New("Audio playback failed"))
					return
				}
			}
		}
	}()
	queue := func(pcm []byte) bool {
		select {
		case playback <- chunk{gen.Load(), pcm}:
			return true
		case <-ctx.Done():
			return false
		}
	}
	responding := "Responding… microphone input paused."
	if bargeIn {
		responding = "Responding… speak to interrupt."
	}
	if awaitingContext {
		fmt.Fprintln(status, "Sending context… microphone input paused.")
	} else if bargeIn {
		fmt.Fprintln(status, "Listening… Speak any time; Ctrl+C ends the session.")
	} else {
		fmt.Fprintln(status, "Listening… Speak between responses; Ctrl+C ends the session.")
	}
	transcriptStarted := false
	audioBytes := 0
	var calls []event // function calls of the current response
	followUp := false // a response.create for tool outputs is pending: its response.created is not an overlap
	muted := false    // the user interrupted: drop audio until the next response
	for ctx.Err() == nil {
		var e event
		if err := c.ReadJSON(&e); err != nil {
			if ctx.Err() == nil {
				fail(errors.New("Realtime connection closed or sent an invalid event"))
			}
			break
		}
		switch e.Type {
		case "error":
			fail(errors.New("Realtime gateway rejected the session or request"))
		case "response.created":
			muted = false
			if !bargeIn && paused.Swap(true) && !awaitingContext && !followUp {
				fail(errors.New("Overlapping responses are not supported"))
				break
			}
			if !awaitingContext && !followUp {
				fmt.Fprintln(status, responding)
			}
			followUp = false
		case "input_audio_buffer.speech_started":
			// The server cancels its response itself (server_vad's interrupt_response); this stops what is queued.
			// ponytail: no conversation.item.truncate, so the model's history keeps the unheard rest of the reply;
			// send it (played bytes / 48 = audio_end_ms) once a provider is confirmed to accept it.
			if bargeIn {
				gen.Add(1)
				if stop, ok := cut.Load().(context.CancelFunc); ok {
					stop()
				}
				muted = true
			}
		case "response.audio.delta", "response.output_audio.delta":
			if !bargeIn {
				paused.Store(true)
			}
			pcm, err := base64.StdEncoding.DecodeString(e.Delta)
			if err != nil || len(pcm) == 0 {
				fail(errors.New("Realtime gateway sent invalid PCM audio"))
				break
			}
			audioBytes += len(pcm)
			for !awaitingContext && !muted && len(pcm) > 0 {
				n := min(frameBytes, len(pcm))
				if !queue(pcm[:n]) {
					break
				}
				pcm = pcm[n:]
			}
		case "response.output_item.done":
			if e.Item.Type == "function_call" && files != nil {
				calls = append(calls, e)
			}
		case "conversation.item.input_audio_transcription.completed":
			if e.Transcript != "" {
				write("You: %s\n", e.Transcript)
			}
		case "response.audio_transcript.delta", "response.output_audio_transcript.delta", "response.text.delta", "response.output_text.delta":
			if awaitingContext { // muted opening reply
				break
			}
			if !transcriptStarted {
				write("Assistant: ")
				transcriptStarted = true
			}
			write("%s", e.Delta)
		case "response.done":
			if e.Response.Status == "cancelled" { // interrupted: its tool calls are not run
				calls = nil
			} else if e.Response.Status != "" && e.Response.Status != "completed" {
				fail(errors.New("Realtime response did not complete"))
				break
			}
			if audioBytes%2 != 0 {
				fail(errors.New("Realtime gateway sent an incomplete PCM sample"))
				break
			}
			audioBytes = 0
			if transcriptStarted {
				write("\n")
				transcriptStarted = false
			}
			if len(calls) > 0 {
				for _, call := range calls {
					output, summary := files.call(call.Item.Name, call.Item.Arguments)
					write("Tool: %s\n", summary)
					if err := send(map[string]any{"type": "conversation.item.create", "item": map[string]any{
						"type": "function_call_output", "call_id": call.Item.CallID, "output": output}}); err != nil {
						fail(errors.New("Could not send a tool result"))
					}
				}
				calls = nil
				if err := send(map[string]string{"type": "response.create"}); err != nil {
					fail(errors.New("Could not send a tool result"))
				}
				followUp = true
				break
			}
			awaitingContext = false
			queue(nil)
		}
	}
	if parent.Err() != nil {
		return nil
	}
	select {
	case err := <-failures:
		return err
	default:
		return nil
	}
}
