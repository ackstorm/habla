// Package stats is `habla stats`: one JSON line per dictation or `habla say` in stats.jsonl, and the report over them.
package stats

import (
	"bufio"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/ackstorm/habla/internal/config"
	"github.com/ackstorm/habla/internal/gateway"
)

// Call is one priced gateway reply.
type Call struct {
	Service string  `json:"service"` // stt, llm or tts
	Model   string  `json:"model"`
	In      int     `json:"in,omitempty"`    // text input tokens
	Audio   int     `json:"audio,omitempty"` // audio input tokens
	Out     int     `json:"out,omitempty"`
	Ms      int64   `json:"ms"`
	Cost    float64 `json:"cost,omitempty"` // USD
	Source  string  `json:"cost_src"`       // header, prices or none
}

// Record is one dictation (kind listen) or `habla say` (kind say). Counts only: never the text.
type Record struct {
	Time   time.Time        `json:"time"`
	Kind   string           `json:"kind"`
	AudioS float64          `json:"audio_s,omitempty"`
	Chars  int              `json:"chars"`
	Words  int              `json:"words"`
	Ms     map[string]int64 `json:"ms,omitempty"` // listen: stt, transform, total (release → delivered)
	Calls  []Call           `json:"calls"`
}

// Path is $XDG_STATE_HOME/habla/stats.jsonl.
func Path() string { return filepath.Join(config.StateDir(), "stats.jsonl") }

// Calls prices replies: the gateway's own cost first, then [prices], else tokens only.
func Calls(calls []gateway.Call, prices map[string]config.Price) []Call {
	out := make([]Call, 0, len(calls))
	for _, c := range calls {
		s := Call{Service: c.Service, Model: c.Model, In: c.In, Audio: c.Audio, Out: c.Out, Ms: c.Ms, Source: "none"}
		if p, ok := prices[c.Model]; c.Costed {
			s.Cost, s.Source = c.Cost, "header"
		} else if ok && c.In+c.Audio+c.Out > 0 {
			s.Cost = (float64(c.In)*p.Input + float64(c.Audio)*p.Audio + float64(c.Out)*p.Output) / 1e6
			s.Source = "prices"
		}
		out = append(out, s)
	}
	return out
}

// Append adds r as one line: one O_APPEND write, so concurrent habla processes never interleave. A failure only
// warns: stats never break a dictation.
func Append(r Record) {
	b, _ := json.Marshal(r)
	path := Path()
	os.MkdirAll(filepath.Dir(path), 0o700)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE|syscall.O_CLOEXEC, 0o600)
	if err == nil {
		_, err = f.Write(append(b, '\n'))
		f.Close()
	}
	if err != nil {
		slog.Warn("stats not recorded", "err", err)
	}
}

// Load reads every record; malformed lines are skipped with one warning.
func Load(path string) ([]Record, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var records []Record
	skipped := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var r Record
		if json.Unmarshal(sc.Bytes(), &r) != nil || r.Time.IsZero() {
			skipped++
			continue
		}
		records = append(records, r)
	}
	if skipped > 0 {
		slog.Warn("stats: malformed lines skipped", "lines", skipped)
	}
	return records, sc.Err()
}
