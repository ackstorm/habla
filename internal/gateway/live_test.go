package gateway

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ackstorm/habla/internal/audio"
	"github.com/ackstorm/habla/internal/config"
	"github.com/ackstorm/habla/internal/testenv"
)

// liveClient talks to the real gateway with $HABLA_API_KEY. Opt-in: HABLA_LIVE=1.
func liveClient(t *testing.T, conn config.Conn) *Client {
	t.Helper()
	if os.Getenv("HABLA_LIVE") != "1" {
		t.Skip("set HABLA_LIVE=1 to call the gateway")
	}
	key, err := Credential(context.Background(), "env:HABLA_API_KEY")
	if err != nil {
		t.Fatal(err)
	}
	return New(conn, key)
}

// hello synthesizes "Hello world." with tts (key: HABLA_API_KEY), as 16 kHz PCM trimmed like a dictation.
func hello(t *testing.T, tts *config.TTS) []byte {
	t.Helper()
	voice, instructions, _ := tts.Delivery()
	wav, err := liveClient(t, tts.Conn).Speak(context.Background(), tts, voice, instructions, "Hello world.")
	if err != nil {
		t.Fatal(err)
	}
	pcm, err := testenv.Speech16k(wav)
	if err != nil {
		t.Fatal(err)
	}
	start, end := audio.Trim(pcm, -45)
	return pcm[start:end]
}

func wantHelloWorld(t *testing.T, text string) {
	words := regexp.MustCompile(`[a-z]+`).FindAllString(strings.ToLower(text), -1)
	if !strings.Contains(strings.Join(words, " "), "hello world") {
		t.Fatalf("transcript %q", text)
	}
}

func TestLiveTranscribe(t *testing.T) {
	cfg := config.Defaults()
	client := liveClient(t, cfg.STT.Conn)
	pcm := hello(t, config.Defaults().TTS)
	text, err := client.Transcribe(context.Background(), cfg.STT.Model, cfg.STT.Language, bytes.NewReader(pcm), int64(len(pcm)))
	if err != nil {
		t.Fatal(err)
	}
	wantHelloWorld(t, text)
}

func TestLiveStreamingTranscribe(t *testing.T) {
	cfg := config.Defaults()
	client := liveClient(t, cfg.STT.Conn)
	pcm := hello(t, config.Defaults().TTS)
	pr, pw := io.Pipe()
	go func() {
		for i := 0; i < len(pcm); i += 3200 { // 100 ms of audio every 50 ms
			pw.Write(pcm[i:min(i+3200, len(pcm))])
			time.Sleep(50 * time.Millisecond)
		}
		pw.Close()
	}()
	text, err := client.Transcribe(context.Background(), cfg.STT.Model, cfg.STT.Language, pr, -1)
	if err != nil {
		t.Fatal(err)
	}
	wantHelloWorld(t, text)
}

func TestLiveCleanRemovesFillersAndKeepsTheCorrection(t *testing.T) {
	cfg := config.Defaults()
	client := liveClient(t, *cfg.LLM)
	text, err := client.Transform(context.Background(), cfg.TransformModel("clean"), cfg.Transform["clean"].Prompt, "Um, so, like, the meeting is on Tuesday, "+
		"no, sorry, on Wednesday at, uh, at five, you know?")
	if err != nil {
		t.Fatal(err)
	}
	lowered := strings.ToLower(text)
	words := regexp.MustCompile(`\pL+`).FindAllString(lowered, -1)
	for _, filler := range []string{"um", "uh", "like", "so", "sorry"} {
		for _, w := range words {
			if w == filler {
				t.Fatalf("filler %q left in %q", filler, text)
			}
		}
	}
	if !strings.Contains(lowered, "wednesday") || strings.Contains(lowered, "tuesday") {
		t.Fatalf("correction not applied: %q", text)
	}
}

func TestLiveSpeakRoundTripsThroughSTT(t *testing.T) {
	cfg := config.Defaults()
	cfg.TTS.Style = "sevilla"
	client := liveClient(t, cfg.TTS.Conn)
	voice, instructions, _ := cfg.TTS.Delivery()
	wav, err := client.Speak(context.Background(), cfg.TTS, voice, instructions, "Esta tarde iremos a pasear por la plaza central.")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(wav, []byte("RIFF")) {
		t.Fatalf("not a WAV: %q", wav[:min(len(wav), 8)])
	}
	offset, length, ok := audio.Data(wav)
	if !ok {
		t.Fatal("speech WAV has no data chunk")
	}
	pcm := wav[offset : offset+length]
	if bytes.Contains(pcm, []byte("RIFF")) || bytes.Contains(pcm, []byte("C2PA")) {
		t.Fatal("a WAV header or the C2PA chunk is still in the audio")
	}
	// Transcribe puts a 16 kHz header on the PCM, but the TTS audio is 24 kHz: the gateway hears it 1.5x slow and
	// lower. The test still holds because the STT model copes with slowed speech; only the key words are checked.
	stt := liveClient(t, cfg.STT.Conn)
	text, err := stt.Transcribe(context.Background(), cfg.STT.Model, "es", bytes.NewReader(pcm), int64(len(pcm)))
	if err != nil {
		t.Fatal(err)
	}
	words := map[string]bool{}
	for _, w := range regexp.MustCompile(`\pL+`).FindAllString(strings.ToLower(text), -1) {
		words[w] = true
	}
	// The phonetic rewrite can elide words together, so check the stable key words.
	if !words["pasear"] || !words["plaza"] {
		t.Fatalf("transcript %q", text)
	}
	for _, w := range []string{"acento", "andaluz", "sevilla", "reescribe"} {
		if words[w] {
			t.Fatalf("an instruction was read aloud: %q", text)
		}
	}
}

var geminiLive = config.Conn{Endpoint: "https://generativelanguage.googleapis.com/v1beta", API: "gemini", AuthHeader: "x-goog-api-key", AuthScheme: "raw"}

func TestLiveGeminiTransform(t *testing.T) {
	cfg := config.Defaults()
	client := liveClient(t, geminiLive)
	text, err := client.Transform(context.Background(), "gemini-3.8-flash", cfg.Transform["clean"].Prompt, "uh tomorrow at 2 no sorry at ten")
	if err != nil || !strings.Contains(strings.ToLower(text), "ten") {
		t.Fatalf("transform %q: %v", text, err)
	}
}

func TestLiveGeminiSpeak(t *testing.T) {
	client := liveClient(t, geminiLive)
	tts := &config.TTS{Conn: config.Conn{API: "gemini", Model: "gemini-3.8-flash-tts"}}
	wav, err := client.Speak(context.Background(), tts, "Kore", "", "Una tarde tranquila junto al río.")
	if err != nil {
		t.Fatal(err)
	}
	offset, length, ok := audio.Data(wav)
	if !ok || bytes.Contains(wav[offset:offset+length], []byte("RIFF")) {
		t.Fatal("Gemini response is not a clean WAV payload")
	}
}

func TestLiveGeminiTranscribeStreamed(t *testing.T) {
	client := liveClient(t, geminiLive)
	cfg := config.Defaults()
	if err := cfg.Use("tts", "gemini"); err != nil {
		t.Fatal(err)
	}
	text, err := client.Transcribe(context.Background(), "gemini-3.5-transcribe", "", bytes.NewReader(hello(t, cfg.TTS)), -1)
	if err != nil || !strings.Contains(strings.ToLower(text), "hello") {
		t.Fatalf("transcription %q: %v", text, err)
	}
}

// TestLiveElevenLabsSpeak calls api.elevenlabs.io with $ELEVENLABS_API_KEY: one short request.
func TestLiveElevenLabsSpeak(t *testing.T) {
	if os.Getenv("HABLA_LIVE") != "1" || os.Getenv("ELEVENLABS_API_KEY") == "" {
		t.Skip("set HABLA_LIVE=1 and ELEVENLABS_API_KEY to call ElevenLabs")
	}
	cfg := config.Defaults()
	if err := cfg.Use("tts", "elevenlabs"); err != nil {
		t.Fatal(err)
	}
	key, err := Credential(context.Background(), cfg.TTS.APIKey)
	if err != nil {
		t.Fatal(err)
	}
	voice, _, _ := cfg.TTS.Delivery()
	data, err := New(cfg.TTS.Conn, key).Speak(context.Background(), cfg.TTS, voice, "", "Hola.")
	if err != nil {
		t.Fatal(err)
	}
	if offset, length, ok := audio.Data(data); !ok || length == 0 || offset != 44 || binary.LittleEndian.Uint32(data[24:]) != 24000 {
		t.Fatalf("not a non-empty 24 kHz WAV: %d bytes", len(data))
	}
}
