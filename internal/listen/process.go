package listen

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/ackstorm/habla/internal/audio"
	"github.com/ackstorm/habla/internal/config"
	"github.com/ackstorm/habla/internal/desktop"
	"github.com/ackstorm/habla/internal/gateway"
	"github.com/ackstorm/habla/internal/rundir"
	"github.com/ackstorm/habla/internal/stats"
)

const RetryHint = "The recording is kept: habla listen --retry"

// dictation is one or more parts recorded one after another and delivered as one text: a part started while the
// earlier ones are still being transcribed or transformed (SIGUSR2, from `habla listen --start`) joins them.
type dictation struct {
	cfg           *config.Config
	desktopNotify bool
	signals       <-chan os.Signal // SIGTERM or Ctrl+C while busy discards the dictation (nil: never)
	more          <-chan os.Signal // SIGUSR2 while busy: record another part (nil: never)
	parts         []*part
}

// part is one recording and its transcript.
type part struct {
	wav  string // the stopped recording (failed.wav for a retry)
	u    *upload
	text string
	err  error
	done chan struct{} // closed once text and err are set
}

// add finishes wav's transcription in the background.
func (d *dictation) add(wav string, u *upload) {
	p := &part{wav: wav, u: u, done: make(chan struct{})}
	go func() { p.text, p.err = u.finish(wav); close(p.done) }()
	d.parts = append(d.parts, p)
}

// finish is the pipeline once no part records: transcripts → join → transform → print → copy → paste (when
// cfg.Paste). A failed transcription keeps the audio as failed.wav. again: SIGUSR2 asked for another part first (a
// transform in flight is dropped); the caller records it and calls finish again.
func (d *dictation) finish() (code int, again bool) {
	start := time.Now() // the release
	rundir.WriteState("busy")
	for _, p := range d.parts {
		if event := d.wait(p.done); event != "" {
			return d.interrupt(event)
		}
	}
	var texts []string
	for _, p := range d.parts {
		if p.err != nil {
			return d.fail(p.err), false
		}
		if text := strings.TrimSpace(p.text); text != "" {
			texts = append(texts, text)
		}
	}
	text := strings.Join(texts, " ")
	sttMs := time.Since(start).Milliseconds()
	slog.Info("stt done", "ms", sttMs, "chars", utf8.RuneCountInString(text), "parts", len(d.parts))
	name, detail := d.cfg.STT.Transform, d.cfg.STT.Model
	var transformErr error
	var transformMs int64
	if text != "" && name != "" && d.cfg.LLM == nil {
		slog.Warn("transform not applied: no [llm]", "transform", name)
	} else if text != "" && name != "" {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel() // drops a transform that another part or a cancel interrupted
		var transformed string
		var err error
		done := make(chan struct{})
		u := d.parts[len(d.parts)-1].u // read here: after an interrupt, record appends to d.parts
		model, prompt := d.cfg.TransformModel(name), d.cfg.Transform[name].Prompt
		if u.llmErr == nil {
			rundir.WriteState("transform")
		}
		transformStart := time.Now()
		go func() {
			if u.llmErr != nil {
				err = u.llmErr
			} else {
				transformed, err = u.llm.Transform(ctx, model, prompt, text)
			}
			close(done)
		}()
		if event := d.wait(done); event != "" {
			return d.interrupt(event)
		}
		transformMs = time.Since(transformStart).Milliseconds()
		if err != nil {
			slog.Warn("transform failed: copying the raw text", "err", err)
			transformErr = err
		} else {
			text, detail = transformed, detail+" · "+name
		}
	}
	audioS, calls := d.usage()
	d.removeParts() // before the release: the next session's files must not be touched
	rundir.Delivery("copying")
	rundir.Release(rundir.Listen) // a new dictation may start while this one is copied
	code = deliver(d.cfg, text, detail, transformErr, d.desktopNotify, start)
	stats.Append(stats.Record{Time: start, Kind: "listen", AudioS: audioS,
		Chars: utf8.RuneCountInString(text), Words: len(strings.Fields(text)),
		Ms:    map[string]int64{"stt": sttMs, "transform": transformMs, "total": time.Since(start).Milliseconds()},
		Calls: stats.Calls(calls, d.cfg.Prices)})
	return code, false
}

// usage is the recordings' length and every gateway reply the parts got, for habla stats.
func (d *dictation) usage() (seconds float64, calls []gateway.Call) {
	header := int64(len(audio.Header(0)))
	for _, p := range d.parts {
		if info, err := os.Stat(p.wav); err == nil {
			seconds += float64(info.Size()-header) / (2 * audio.Rate)
		}
		for _, c := range []*gateway.Client{p.u.client, p.u.llm} {
			if c != nil {
				calls = append(calls, c.Calls()...)
			}
		}
	}
	return seconds, calls
}

// wait returns "" once done is closed, or "more" (SIGUSR2) or "cancel" (SIGTERM, Ctrl+C) if one comes first.
func (d *dictation) wait(done <-chan struct{}) string {
	for {
		select {
		case <-done:
			return ""
		case <-d.more:
			return "more"
		case s := <-d.signals:
			if s != syscall.SIGUSR1 { // a late --stop has nothing to stop
				return "cancel"
			}
		}
	}
}

// interrupt ends a wait cut short: "more" records another part; "cancel" discards the dictation.
func (d *dictation) interrupt(event string) (code int, again bool) {
	if event == "more" {
		slog.Info("another part", "part", len(d.parts)+1)
		return 0, true
	}
	for _, p := range d.parts {
		p.u.abort()
	}
	d.removeParts()
	rundir.WriteState("")
	slog.Info("dictation discarded", "parts", len(d.parts))
	fmt.Fprintln(os.Stderr, "Discarded.")
	return 1, false
}

// fail keeps the dictation's audio as failed.wav for --retry (the parts' PCM, joined; the newest failure replaces
// an older one) and reports err.
func (d *dictation) fail(err error) int {
	slog.Error("stt failed", "parts", len(d.parts), "err", err)
	failed := filepath.Join(rundir.Dir(), "failed.wav")
	if len(d.parts) == 1 {
		os.Rename(d.parts[0].wav, failed)
	} else if err := joinWAVs(failed, d.parts); err != nil {
		slog.Error("could not keep the audio", "err", err) // ponytail: the parts stay in the runtime dir
	} else {
		d.removeParts()
	}
	rundir.WriteState("failed")
	desktop.Notify(err.Error(), RetryHint, d.desktopNotify)
	return 1
}

// joinWAVs writes the parts' PCM, in order, to path as one WAV.
func joinWAVs(path string, parts []*part) error {
	var pcm []byte
	for _, p := range parts {
		data, err := os.ReadFile(p.wav)
		if err != nil {
			return err
		}
		offset, length, ok := audio.Data(data)
		if !ok {
			return fmt.Errorf("%s: not a WAV file", p.wav)
		}
		pcm = append(pcm, data[offset:offset+length]...)
	}
	return os.WriteFile(path, append(audio.Header(int64(len(pcm))), pcm...), 0o600)
}

// removeParts deletes the parts' recordings.
func (d *dictation) removeParts() {
	for _, p := range d.parts {
		os.Remove(p.wav)
	}
}

// deliver prints text, copies it and pastes it (when cfg.Paste, then Enter with cfg.PasteEnter), then says so. start is the release.
func deliver(cfg *config.Config, text, detail string, transformErr error, desktopNotify bool, start time.Time) int {
	if os.Remove(rundir.SendMark()) == nil { // Enter while busy (--send)
		cfg.Paste, cfg.PasteEnter = true, true
	}
	if text == "" {
		rundir.Delivery("")
		desktop.Notify("Nothing heard.", "", desktopNotify)
		return 1
	}
	fmt.Println(text)
	if err := desktop.Copy(text); err != nil {
		rundir.Delivery("copy_failed")
		slog.Error("copy failed", "err", err)
		desktop.Notify("Copy failed: "+err.Error(), text, desktopNotify)
		return 1
	}
	slog.Info("release to copied", "ms", time.Since(start).Milliseconds())
	var pasteErr error
	if cfg.Paste && transformErr == nil {
		rundir.Delivery("pasting")
		pasteErr = desktop.Paste(cfg.PasteKeys, cfg.PasteEnter)
	}
	rundir.Delivery("") // desktop notifications must not keep a completed delivery busy
	switch {
	case transformErr != nil: // raw speech is never pasted: the user asked for a transformed text
		desktop.Notify("Transform failed: raw text copied", transformErr.Error(), desktopNotify)
	case cfg.Paste:
		if pasteErr != nil {
			slog.Warn("paste failed: the text is on the clipboard", "err", pasteErr)
			desktop.Notify("Copied: press Ctrl+V to paste", pasteErr.Error(), desktopNotify)
		} else if cfg.PasteEnter {
			desktop.Notify("Pasted and sent", detail, desktopNotify)
		} else {
			desktop.Notify("Pasted", detail, desktopNotify)
		}
	default:
		words, s := len(strings.Fields(text)), "s"
		if words == 1 {
			s = ""
		}
		desktop.Notify(fmt.Sprintf("Copied %d word%s", words, s), detail, desktopNotify)
	}
	return 0
}
