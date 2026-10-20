package live

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
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
		"The project codename is COBALT. When I say hello world, reply with only the project codename.", config.Delivery{Instructions: "Keep every answer short and speak with a calm delivery."})
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
	}, opening, &transcript, listening(ready))
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
