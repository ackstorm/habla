package gateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"github.com/ackstorm/habla/internal/audio"
	"github.com/ackstorm/habla/internal/config"
)

func geminiClient(t *testing.T, reply string) (*Client, func() []request) {
	t.Helper()
	c, seen := serve(t, 200, reply)
	c.conn.API = "gemini"
	return c, seen
}

func TestGeminiTransformRequestAndReply(t *testing.T) {
	c, seen := geminiClient(t, `{"candidates":[{"content":{"parts":[{"text":" Hola. "}]}}]}`)
	out, err := c.Transform(context.Background(), "gemini-3.8-flash", "Limpia.", "eh hola")
	if err != nil || out != "Hola." {
		t.Fatal(out, err)
	}
	r := seen()[0]
	if r.path != "/v1/models/gemini-3.8-flash:generateContent" {
		t.Fatal(r.path)
	}
	var got map[string]any
	if err := json.Unmarshal(r.body, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"systemInstruction": map[string]any{"parts": []any{map[string]any{"text": "Limpia.\n\n" + TransformSuffix}}},
		"contents":          []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": "<transcript>eh hola</transcript>"}}}},
		"generationConfig":  map[string]any{"temperature": 0.0, "thinkingConfig": map[string]any{"thinkingBudget": 0.0}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%v", got)
	}
}

func TestGeminiSpeakModes(t *testing.T) {
	wav := base64.StdEncoding.EncodeToString([]byte("RIFF-real-wav"))
	for _, tc := range []struct {
		mode, instructions, wantText string
		wantSystem                   bool
	}{
		{"", "", "Hola.", false},
		{"inline", "Andaluz.", fmt.Sprintf(InlineTemplate, "Andaluz.", "Hola."), false},
		{"system_instruction", "Andaluz.", "Hola.", true},
	} {
		c, seen := geminiClient(t, `{"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"audio/wav","data":"`+wav+`"}}]}}]}`)
		tts := &config.TTS{Conn: config.Conn{Model: "gemini-3.8-flash-tts", API: "gemini"}, InstructionsMode: tc.mode}
		data, err := c.Speak(context.Background(), tts, "Kore", tc.instructions, "Hola.")
		if err != nil || string(data) != "RIFF-real-wav" {
			t.Fatal(string(data), err)
		}
		var got struct {
			SystemInstruction *struct{ Parts []struct{ Text string } }  `json:"systemInstruction"`
			Contents          []struct{ Parts []struct{ Text string } } `json:"contents"`
			GenerationConfig  struct {
				ResponseModalities []string `json:"responseModalities"`
				SpeechConfig       struct {
					VoiceConfig struct {
						PrebuiltVoiceConfig struct {
							VoiceName string `json:"voiceName"`
						} `json:"prebuiltVoiceConfig"`
					} `json:"voiceConfig"`
				} `json:"speechConfig"`
			} `json:"generationConfig"`
		}
		if err := json.Unmarshal(seen()[0].body, &got); err != nil {
			t.Fatal(err)
		}
		if got.Contents[0].Parts[0].Text != tc.wantText || (got.SystemInstruction != nil) != tc.wantSystem ||
			!reflect.DeepEqual(got.GenerationConfig.ResponseModalities, []string{"AUDIO"}) || got.GenerationConfig.SpeechConfig.VoiceConfig.PrebuiltVoiceConfig.VoiceName != "Kore" {
			t.Errorf("%s: %+v", tc.mode, got)
		}
	}
}

func TestGeminiSpeakRejectsOtherAudioTypes(t *testing.T) {
	c, _ := geminiClient(t, `{"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"audio/L16;rate=24000","data":"AAAA"}}]}}]}`)
	_, err := c.Speak(context.Background(), &config.TTS{Conn: config.Conn{Model: "m", API: "gemini"}}, "Kore", "", "Hola.")
	if err == nil || err.Error() != "Gateway reply audio is audio/L16;rate=24000, want audio/wav" {
		t.Fatal(err)
	}
}

func TestGeminiTranscribeStreamsTheWAVAsBase64(t *testing.T) {
	c, seen := geminiClient(t, `{"candidates":[{"content":{"parts":[{"audioTranscription":{"text":"Hello world."}}]}}]}`)
	pcm := bytes.Repeat([]byte{1, 2, 3, 4, 5}, 1000)
	text, err := c.Transcribe(context.Background(), "gemini-3.5-transcribe", "es", bytes.NewReader(pcm), -1)
	if err != nil || text != "Hello world." {
		t.Fatal(text, err)
	}
	var got struct {
		Contents []struct {
			Role  string
			Parts []struct {
				InlineData struct {
					MimeType string `json:"mimeType"`
					Data     []byte `json:"data"`
				} `json:"inlineData"`
			} `json:"parts"`
		} `json:"contents"`
	}
	if err := json.Unmarshal(seen()[0].body, &got); err != nil {
		t.Fatal(err)
	}
	d := got.Contents[0].Parts[0].InlineData
	wav := append(audio.Header(-1), pcm...)
	wantEncoded := `"data":"` + base64.StdEncoding.EncodeToString(wav) + `"`
	if got.Contents[0].Role != "user" || d.MimeType != "audio/wav" || !bytes.Equal(d.Data, wav) || !bytes.Contains(seen()[0].body, []byte(wantEncoded)) {
		t.Fatal("want the streaming header and the PCM, base64 in inlineData")
	}
	if bytes.Contains(seen()[0].body, []byte(`"language"`)) {
		t.Fatal("stt.language is not sent to Gemini")
	}
}
