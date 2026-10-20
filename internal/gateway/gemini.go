package gateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/ackstorm/habla/internal/audio"
	"github.com/ackstorm/habla/internal/config"
)

type geminiReply struct {
	Candidates []struct {
		Content struct {
			Parts []geminiPart `json:"parts"`
		} `json:"content"`
	} `json:"candidates"`
}

type geminiPart struct {
	Text               string `json:"text"`
	AudioTranscription *struct {
		Text string `json:"text"`
	} `json:"audioTranscription"`
	InlineData *struct {
		MimeType string `json:"mimeType"`
		Data     []byte `json:"data"`
	} `json:"inlineData"`
}

func (c *Client) generate(ctx context.Context, service, model string, body io.Reader, timeout time.Duration) ([]geminiPart, error) {
	var reply geminiReply
	if err := c.post(ctx, service, model, "/models/"+model+":generateContent", "application/json", body, timeout, &reply); err != nil {
		return nil, err
	}
	if len(reply.Candidates) == 0 {
		return nil, &Error{Msg: "Gateway reply has no candidates"}
	}
	return reply.Candidates[0].Content.Parts, nil
}

func (c *Client) geminiTranscribe(ctx context.Context, model string, pcm io.Reader, size int64) (string, error) {
	body, w := io.Pipe()
	go func() {
		err := func() error {
			if _, err := io.WriteString(w, `{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"audio/wav","data":"`); err != nil {
				return err
			}
			enc := base64.NewEncoder(base64.StdEncoding, w)
			if _, err := enc.Write(audio.Header(size)); err != nil {
				return err
			}
			if _, err := io.Copy(enc, pcm); err != nil {
				return err
			}
			if err := enc.Close(); err != nil {
				return err
			}
			_, err := io.WriteString(w, `"}}]}]}`)
			return err
		}()
		_ = w.CloseWithError(err)
	}()
	defer body.Close()
	parts, err := c.generate(ctx, "stt", model, body, TranscribeTimeout)
	if err != nil {
		return "", err
	}
	return geminiText(parts), nil
}

func geminiText(parts []geminiPart) string {
	var b strings.Builder
	for _, p := range parts {
		if p.AudioTranscription != nil {
			b.WriteString(p.AudioTranscription.Text)
		}
		b.WriteString(p.Text)
	}
	return b.String()
}

func (c *Client) geminiTransform(ctx context.Context, model, system, user string) (string, error) {
	req, _ := json.Marshal(map[string]any{
		"systemInstruction": map[string]any{"parts": []any{map[string]string{"text": system}}},
		"contents":          []any{map[string]any{"role": "user", "parts": []any{map[string]string{"text": user}}}},
		"generationConfig":  map[string]any{"temperature": 0, "thinkingConfig": map[string]any{"thinkingBudget": 0}},
	})
	parts, err := c.generate(ctx, "llm", model, bytes.NewReader(req), TransformTimeout)
	if err != nil {
		return "", err
	}
	if out := strings.TrimSpace(geminiText(parts)); out != "" {
		return out, nil
	}
	return "", &Error{Msg: "Gateway reply has no transform text"}
}

func (c *Client) geminiSpeak(ctx context.Context, t *config.TTS, voice, instructions, text string) ([]byte, error) {
	body := map[string]any{
		"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]string{"text": text}}}},
		"generationConfig": map[string]any{
			"responseModalities": []string{"AUDIO"},
			"speechConfig":       map[string]any{"voiceConfig": map[string]any{"prebuiltVoiceConfig": map[string]string{"voiceName": voice}}},
		},
	}
	switch {
	case instructions == "":
	case t.InstructionsMode == "inline":
		body["contents"] = []any{map[string]any{"role": "user", "parts": []map[string]string{{"text": fmt.Sprintf(InlineTemplate, instructions, text)}}}}
	case t.InstructionsMode == "system_instruction":
		body["systemInstruction"] = map[string]any{"parts": []any{map[string]string{"text": instructions}}}
	}
	req, _ := json.Marshal(body)
	parts, err := c.generate(ctx, "tts", t.Model, bytes.NewReader(req), SpeechTimeout)
	if err != nil {
		return nil, err
	}
	for _, p := range parts {
		if p.InlineData == nil {
			continue
		}
		if p.InlineData.MimeType != "audio/wav" {
			return nil, &Error{Msg: "Gateway reply audio is " + p.InlineData.MimeType + ", want audio/wav"}
		}
		return p.InlineData.Data, nil
	}
	return nil, &Error{Msg: "Gateway reply has no audio"}
}
