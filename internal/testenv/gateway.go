package testenv

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/ackstorm/habla/internal/config"
)

// Replies are what the fake gateway answers. A status other than 200 answers with an error body.
type Replies struct {
	Transcript, Transformed string
	STTStatus, ChatStatus   int
	Speech                  []byte        // the /audio/speech reply
	SpeechStatus            int           // 0 means 200
	HoldSTT, HoldChat       chan struct{} // when set, transcriptions / chat requests wait for a close (or the client leaving)
	Cost                    string        // x-litellm-response-cost on every reply ("" = none)
}

var DefaultReplies = Replies{Transcript: "Eh, pues hola.", Transformed: "Hola.", STTStatus: 200, ChatStatus: 200, Speech: []byte("RIFF-fake-speech")}

// Gateway is a fake gateway. It keeps the paths it was asked and the uploaded WAV of each transcription;
// Started receives once per transcription request, when its first audio byte arrives.
type Gateway struct {
	URL     string
	Started chan struct{}
	Aborted chan struct{} // receives when a transcription's upload ended before its end (the client gave up)
	replies Replies
	mu      sync.Mutex
	paths   []string
	bodies  [][]byte // the JSON body of each non-transcription request, aligned with paths
	audio   [][]byte
}

func NewGateway(t *testing.T, r Replies) *Gateway {
	t.Helper()
	g := &Gateway{replies: r, Started: make(chan struct{}, 10), Aborted: make(chan struct{}, 10)}
	srv := httptest.NewServer(http.HandlerFunc(g.serve))
	t.Cleanup(srv.Close)
	g.URL = srv.URL + "/v1"
	return g
}

func (g *Gateway) Paths() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string{}, g.paths...)
}

// Body returns the JSON body of the n-th request (nil for a transcription).
func (g *Gateway) Body(n int) []byte {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.bodies[n]
}

func (g *Gateway) Audio() [][]byte {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([][]byte{}, g.audio...)
}

func (g *Gateway) serve(w http.ResponseWriter, r *http.Request) {
	if g.replies.Cost != "" {
		w.Header().Set("x-litellm-response-cost", g.replies.Cost)
	}
	if strings.HasSuffix(r.URL.Path, ":generateContent") {
		g.mu.Lock()
		index := len(g.paths)
		g.paths = append(g.paths, r.URL.Path)
		g.bodies = append(g.bodies, nil)
		g.mu.Unlock()
		g.serveGemini(w, r, index)
		return
	}
	var body []byte
	if !strings.HasSuffix(r.URL.Path, "/audio/transcriptions") {
		body, _ = io.ReadAll(r.Body)
	}
	g.mu.Lock()
	g.paths = append(g.paths, r.URL.Path)
	g.bodies = append(g.bodies, body)
	g.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if strings.HasSuffix(r.URL.Path, "/audio/speech") {
		if g.replies.SpeechStatus != 0 {
			w.WriteHeader(g.replies.SpeechStatus)
			fmt.Fprint(w, `{"error": {"message": "voice not supported"}}`)
			return
		}
		w.Header().Set("Content-Type", "audio/wav")
		w.Write(g.replies.Speech)
		return
	}
	if body != nil {
		hold(r, g.replies.HoldChat)
		if g.replies.ChatStatus != 200 {
			w.WriteHeader(g.replies.ChatStatus)
			fmt.Fprint(w, `{"error": {"message": "transform down"}}`)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]string{"content": g.replies.Transformed}}},
			"usage":   map[string]int{"prompt_tokens": 50, "completion_tokens": 5},
		})
		return
	}
	wav, err := g.readAudio(r)
	if err != nil { // the client gave up (a discarded recording)
		select {
		case g.Aborted <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	g.mu.Lock()
	g.audio = append(g.audio, wav)
	g.mu.Unlock()
	hold(r, g.replies.HoldSTT)
	if g.replies.STTStatus != 200 {
		w.WriteHeader(g.replies.STTStatus)
		fmt.Fprint(w, `{"error": {"message": "no access"}}`)
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"text": g.replies.Transcript, "usage": map[string]any{
		"type": "tokens", "input_tokens": 120, "input_token_details": map[string]int{"audio_tokens": 100}, "output_tokens": 8}})
}

func (g *Gateway) serveGemini(w http.ResponseWriter, r *http.Request, index int) {
	const marker = `"data":"`
	br := bufio.NewReader(r.Body)
	var head []byte
	for !bytes.HasSuffix(head, []byte(marker)) {
		b, err := br.ReadByte()
		if err != nil {
			break
		}
		head = append(head, b)
	}
	streamingAudio := bytes.HasSuffix(head, []byte(marker)) && bytes.Contains(head, []byte(`"inlineData"`))
	if streamingAudio {
		if b, err := br.ReadByte(); err == nil {
			head = append(head, b)
			select {
			case g.Started <- struct{}{}:
			default:
			}
		}
	}
	rest, err := io.ReadAll(br)
	body := append(head, rest...)
	var req struct {
		Contents []struct {
			Parts []struct {
				InlineData *struct {
					Data []byte `json:"data"`
				} `json:"inlineData"`
			} `json:"parts"`
		} `json:"contents"`
		GenerationConfig struct {
			ResponseModalities []string `json:"responseModalities"`
		} `json:"generationConfig"`
	}
	if err != nil || json.Unmarshal(body, &req) != nil {
		select {
		case g.Aborted <- struct{}{}:
		default:
		}
		http.Error(w, "invalid Gemini body", http.StatusBadRequest)
		return
	}
	reply := func(part map[string]any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"candidates":    []any{map[string]any{"content": map[string]any{"parts": []any{part}}}},
			"usageMetadata": map[string]any{"promptTokenCount": 130, "candidatesTokenCount": 4, "promptTokensDetails": []any{map[string]any{"modality": "AUDIO", "tokenCount": 100}}},
		})
	}
	switch {
	case len(req.Contents) > 0 && len(req.Contents[0].Parts) > 0 && req.Contents[0].Parts[0].InlineData != nil:
		g.recordGeminiBody(index, nil)
		g.mu.Lock()
		g.audio = append(g.audio, req.Contents[0].Parts[0].InlineData.Data)
		g.mu.Unlock()
		hold(r, g.replies.HoldSTT)
		if g.replies.STTStatus != 200 {
			w.WriteHeader(g.replies.STTStatus)
			fmt.Fprint(w, `{"error":{"message":"no access"}}`)
			return
		}
		reply(map[string]any{"audioTranscription": map[string]string{"text": g.replies.Transcript}})
	case slices.Contains(req.GenerationConfig.ResponseModalities, "AUDIO"):
		g.recordGeminiBody(index, body)
		if g.replies.SpeechStatus != 0 {
			w.WriteHeader(g.replies.SpeechStatus)
			fmt.Fprint(w, `{"error":{"message":"voice not supported"}}`)
			return
		}
		reply(map[string]any{"inlineData": map[string]string{"mimeType": "audio/wav", "data": base64.StdEncoding.EncodeToString(g.replies.Speech)}})
	default:
		g.recordGeminiBody(index, body)
		hold(r, g.replies.HoldChat)
		if g.replies.ChatStatus != 200 {
			w.WriteHeader(g.replies.ChatStatus)
			fmt.Fprint(w, `{"error":{"message":"transform down"}}`)
			return
		}
		reply(map[string]any{"text": g.replies.Transformed})
	}
}

func (g *Gateway) recordGeminiBody(index int, body []byte) {
	g.mu.Lock()
	g.bodies[index] = body
	g.mu.Unlock()
}

// hold waits for c to close, or for the client to give up; nil does not wait.
func hold(r *http.Request, c chan struct{}) {
	if c != nil {
		select {
		case <-c:
		case <-r.Context().Done():
		}
	}
}

func (g *Gateway) readAudio(r *http.Request) ([]byte, error) {
	mr, err := r.MultipartReader()
	if err != nil {
		return nil, err
	}
	for {
		part, err := mr.NextPart()
		if err != nil {
			return nil, err
		}
		if part.FormName() != "file" {
			continue
		}
		first := make([]byte, 1)
		if _, err := io.ReadFull(part, first); err != nil {
			return nil, err
		}
		select {
		case g.Started <- struct{}{}:
		default:
		}
		rest, err := io.ReadAll(part)
		return append(first, rest...), err
	}
}

// WriteConfig writes the generated config with the fake gateway and test credential source.
func (e *Env) WriteConfig(t *testing.T, g *Gateway, extra string) {
	t.Helper()
	text := strings.ReplaceAll(config.DefaultTOML, `"https://api.openai.com/v1"`, strconv.Quote(g.URL))
	text = strings.ReplaceAll(text, `"env:OPENAI_API_KEY"`, `"env:HABLA_API_KEY"`)
	text = strings.ReplaceAll(text, `"wss://api.openai.com/v1/realtime"`, `"wss://unused.invalid/v1/realtime"`)
	e.WriteConfigText(t, Merge(text, extra))
}

// WriteConfigText writes config.toml exactly.
func (e *Env) WriteConfigText(t *testing.T, text string) {
	t.Helper()
	path := filepath.Join(e.Dir, "config", "habla", "config.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Merge overlays TOML section keys: extra values replace matching keys and new sections follow base.
func Merge(base, extra string) string {
	type section struct {
		name  string
		lines []string
	}
	split := func(text string) []*section {
		out := []*section{{}}
		for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
			if s := strings.TrimSpace(line); strings.HasPrefix(s, "[") {
				out = append(out, &section{name: s})
				continue
			}
			out[len(out)-1].lines = append(out[len(out)-1].lines, line)
		}
		return out
	}
	key := func(line string) string { k, _, _ := strings.Cut(line, "="); return strings.TrimSpace(k) }
	merged := split(base)
	for _, es := range split(extra) {
		i := slices.IndexFunc(merged, func(s *section) bool { return s.name == es.name })
		if i < 0 {
			merged = append(merged, &section{name: es.name, lines: slices.Clone(es.lines)})
			continue
		}
		for _, line := range es.lines {
			if strings.TrimSpace(line) == "" {
				continue
			}
			s := merged[i]
			if j := slices.IndexFunc(s.lines, func(l string) bool { return key(l) == key(line) }); j >= 0 {
				s.lines[j] = line
				continue
			}
			end := len(s.lines)
			for end > 0 && strings.TrimSpace(s.lines[end-1]) == "" {
				end--
			}
			s.lines = slices.Insert(s.lines, end, line)
		}
	}
	var b strings.Builder
	for i, s := range merged {
		if s.name != "" {
			if i > 0 && b.Len() > 0 && !strings.HasSuffix(b.String(), "\n\n") {
				b.WriteByte('\n')
			}
			b.WriteString(s.name + "\n")
		}
		for _, line := range s.lines {
			b.WriteString(line + "\n")
		}
	}
	return b.String()
}
