package listen

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ackstorm/habla/internal/audio"
	"github.com/ackstorm/habla/internal/config"
	"github.com/ackstorm/habla/internal/gateway"
)

// holdBack is the audio kept unsent while recording: on release the trailing trim applies to it, and at most this
// much is left to upload. 500 ms of 16 kHz mono s16.
const holdBack = 16000

// minRecording is the shortest recording Habla sends: a tap of the hotkey is not a dictation. 300 ms.
const minRecording = 9600

// upload gets the API key while the recording runs and, once speech starts, streams current.wav to the gateway as
// the recorder writes it.
type upload struct {
	cfg      *config.Config
	wav      string
	ready    chan struct{} // closed once both credentials are resolved
	sttReady chan struct{} // closed once client or keyErr is set
	client   *gateway.Client
	keyErr   error
	llm      *gateway.Client
	llmErr   error
	recorded chan string     // finish sends the recording's final path: the recorder has stopped
	result   chan transcript // the streaming outcome; nil when not streaming
	cancel   context.CancelFunc
}

type transcript struct {
	text     string
	err      error
	streamed bool // false: nothing was sent (no speech found), so finish uploads the file
}

func startUpload(cfg *config.Config, wav string) *upload {
	u := &upload{cfg: cfg, wav: wav, ready: make(chan struct{}), sttReady: make(chan struct{}), recorded: make(chan string, 1)}
	go func() {
		defer close(u.ready)
		stt := cfg.STT.Conn
		transform := cfg.LLM != nil && cfg.STT.Transform != ""
		var llmKey string
		var wg sync.WaitGroup
		if transform && cfg.LLM.APIKey != stt.APIKey {
			wg.Go(func() { llmKey, u.llmErr = gateway.Credential(context.Background(), cfg.LLM.APIKey) })
		}
		key, err := gateway.Credential(context.Background(), stt.APIKey)
		u.keyErr = err
		if err == nil {
			u.client = gateway.New(stt, key)
		}
		close(u.sttReady)
		wg.Wait()
		if transform && cfg.LLM.APIKey == stt.APIKey {
			llmKey, u.llmErr = key, err
		}
		if transform && u.llmErr == nil {
			u.llm = gateway.New(*cfg.LLM, llmKey)
		}
	}()
	return u
}

// streamWhileRecording starts streaming the recording. Call it once, right after startUpload, from a session.
func (u *upload) streamWhileRecording() {
	ctx, cancel := context.WithCancel(context.Background())
	u.cancel, u.result = cancel, make(chan transcript, 1)
	go func() {
		<-u.sttReady
		if u.keyErr != nil {
			u.result <- transcript{}
			return
		}
		u.result <- u.stream(ctx)
	}()
}

// abort cancels the streaming request: the recording was discarded or failed.
func (u *upload) abort() {
	if u.cancel != nil {
		u.cancel()
	}
}

// finish returns the transcript of wav, the stopped recording (renamed since recording started), once the recorder
// has stopped: the streaming request's, else (no speech found, a retry, or a failed stream) a normal upload of wav.
func (u *upload) finish(wav string) (string, error) {
	defer func() { <-u.ready }() // the caller needs the cleanup client and its error after transcription
	u.recorded <- wav
	<-u.sttReady
	if u.keyErr != nil {
		return "", u.keyErr
	}
	if u.result != nil {
		// A stalled connection blocks stream's Write, and the reply timeout only starts at the end of the body:
		// give up on the stream after the same time and fall back.
		t := time.AfterFunc(gateway.TranscribeTimeout, u.abort)
		r := <-u.result
		t.Stop()
		if r.streamed && r.err == nil {
			return r.text, nil
		}
		if r.streamed {
			slog.Warn("stream failed: uploading the file", "err", r.err)
			u.abort() // the other pieces' requests
		}
		// ponytail: any streaming failure gets one normal upload; a proxy may cut a long chunked body.
	}
	return transcribeFile(context.Background(), u.client, u.cfg.STT.Model, u.cfg.STT.Language, wav, u.cfg.TrimBelowDB)
}

// stream tails the WAV file. Nothing is sent before the first voiced frame (minus the padding, where audio.Trim
// cuts), and the last holdBack bytes stay unsent until the recorder stops; then Habla re-reads the finalized file
// and sends up to the trimmed end. Habla sends its own streaming header, never the recorder's. With
// [split], a long pause ends the request (a piece) and the next voiced frame starts another one.
func (u *upload) stream(ctx context.Context) transcript {
	var (
		f       *os.File
		buf     []byte // the WAV file so far
		begin   = -1   // PCM offset the upload starts at, once speech is found
		scanned int    // frames already checked for speech
		sent    int    // PCM offset sent up to
		pcmW    *io.PipeWriter
		reply   chan transcript
		start   time.Time // of the streaming request
		final   string    // the recording's path once the recorder has stopped
		split   = newSplitter(u.cfg)
		pieces  []chan transcript // the requests of the pieces cut so far
	)
	defer func() {
		if f != nil {
			f.Close()
		}
	}()
	tick := time.NewTicker(time.Duration(u.cfg.UploadIntervalMS) * time.Millisecond) // stopping ends the wait at once
	defer tick.Stop()
	for {
		stopped := false
		select {
		case <-ctx.Done():
			if pcmW != nil {
				pcmW.CloseWithError(ctx.Err())
				<-reply
			}
			return transcript{err: ctx.Err(), streamed: begin >= 0 || len(pieces) > 0}
		case final = <-u.recorded:
			stopped = true
		case <-tick.C:
		}
		if stopped {
			var err error
			if buf, err = os.ReadFile(final); err != nil { // the header is final now: exact data length, no trailing chunk
				if pcmW != nil {
					pcmW.CloseWithError(err)
					<-reply
				}
				return transcript{err: err, streamed: true} // finish falls back to a normal upload
			}
		} else {
			if f == nil {
				f, _ = os.Open(u.wav) // exists once the recorder has written its header
			}
			if f != nil {
				buf = readMore(f, buf)
			}
		}
		pcm := pcmOf(buf)
		if len(pcm) < minRecording { // only before the first request: pcm only grows
			if stopped {
				return transcript{} // a tap: transcribeFile sends nothing
			}
			continue
		}
		if begin < 0 {
			begin, scanned = speechStart(pcm, scanned, u.cfg.TrimBelowDB)
			if begin < 0 {
				if stopped {
					return joined(pieces) // no pieces: no speech found, finish uploads the whole file
				}
				continue
			}
			sent, start = begin, time.Now()
			pcmW, reply = u.request(ctx)
			split.start(scanned)
		}
		if cut := split.cut(pcm); cut >= 0 && !stopped {
			cut = max(cut, sent)
			_, err := pcmW.Write(pcm[sent:cut])
			pcmW.Close()
			pieces = append(pieces, reply)
			if err != nil {
				return joined(pieces) // the request ended early: its error
			}
			slog.Info("piece cut", "piece", len(pieces), "seconds", float64(cut-begin)/(2*audio.Rate))
			begin, scanned, pcmW = -1, split.scanned, nil
			continue
		}
		end := len(pcm) - holdBack
		if stopped {
			_, end = audio.Trim(pcm, u.cfg.TrimBelowDB)
			end = max(end, sent)
		}
		if end > sent {
			if _, err := pcmW.Write(pcm[sent:end]); err != nil {
				return joined(append(pieces, reply)) // the request ended early: its error
			}
			slog.Debug("chunk", "bytes", end-sent, "total", end-begin, "ms", time.Since(start).Milliseconds())
			sent = end
		}
		if stopped {
			pcmW.Close()
			return joined(append(pieces, reply))
		}
	}
}

// joined waits for the pieces' transcripts and joins them in order. One failure fails them all (finish then uploads
// the file). No pieces: nothing was sent.
func joined(pieces []chan transcript) transcript {
	if len(pieces) == 0 {
		return transcript{}
	}
	var texts []string
	for _, p := range pieces {
		t := <-p
		if t.err != nil {
			return t
		}
		if text := strings.TrimSpace(t.text); text != "" {
			texts = append(texts, text)
		}
	}
	return transcript{text: strings.Join(texts, " "), streamed: true}
}

// splitter finds the long pauses that end a piece ([split]). nil: split is off.
type splitter struct {
	after, pause float64 // seconds
	belowDB      int
	first, last  int // the piece's first and last voiced frames
	scanned      int // the next frame to check
}

func newSplitter(cfg *config.Config) *splitter {
	if !cfg.Split.Enabled || cfg.TrimBelowDB == 0 { // pauses are found with the trim threshold
		return nil
	}
	return &splitter{after: cfg.Split.After, pause: cfg.Split.Pause, belowDB: cfg.TrimBelowDB}
}

// start begins a piece at its first voiced frame.
func (s *splitter) start(voiced int) {
	if s != nil {
		s.first, s.last, s.scanned = voiced, voiced, voiced
	}
}

// cut scans the frames since the last call and returns where the piece ends, or -1: the end of its last voiced frame
// plus the padding (where audio.Trim cuts), once the piece lasts after seconds and the silence since reaches pause.
func (s *splitter) cut(pcm []byte) int {
	if s == nil {
		return -1
	}
	const frameSeconds = float64(audio.FrameBytes) / (2 * audio.Rate)
	for ; (s.scanned+1)*audio.FrameBytes <= len(pcm); s.scanned++ {
		if audio.Voiced(pcm[s.scanned*audio.FrameBytes:(s.scanned+1)*audio.FrameBytes], s.belowDB) {
			s.last = s.scanned
		} else if float64(s.scanned-s.last)*frameSeconds >= s.pause && float64(s.scanned+1-s.first)*frameSeconds >= s.after {
			s.scanned++
			return min(len(pcm), (s.last+1+audio.PadFrames)*audio.FrameBytes)
		}
	}
	return -1
}

// request starts the streaming transcription; the PCM written to the returned pipe is its body.
func (u *upload) request(ctx context.Context) (*io.PipeWriter, chan transcript) {
	r, w := io.Pipe()
	reply := make(chan transcript, 1)
	go func() {
		text, err := u.client.Transcribe(ctx, u.cfg.STT.Model, u.cfg.STT.Language, r, -1)
		r.CloseWithError(io.ErrClosedPipe) // a request that ended early makes the next write fail
		reply <- transcript{text: text, err: err, streamed: true}
	}()
	return w, reply
}

// speechStart scans pcm's frames from scanned on. begin is where the upload starts (the first voiced frame minus
// the padding, as audio.Trim cuts) or -1 while none is voiced. With trimming off the upload starts at once.
func speechStart(pcm []byte, scanned, belowDB int) (begin, next int) {
	if belowDB == 0 {
		return 0, scanned
	}
	for ; (scanned+1)*audio.FrameBytes <= len(pcm); scanned++ {
		if audio.Voiced(pcm[scanned*audio.FrameBytes:(scanned+1)*audio.FrameBytes], belowDB) {
			return max(0, scanned-audio.PadFrames) * audio.FrameBytes, scanned
		}
	}
	return -1, scanned
}

// pcmOf is the PCM in the WAV bytes so far (nil before the data chunk header).
func pcmOf(wav []byte) []byte {
	offset, length, ok := audio.Data(wav)
	if !ok {
		return nil
	}
	return wav[offset : offset+length]
}

// readMore appends what the recorder wrote since the last read.
func readMore(f *os.File, buf []byte) []byte {
	chunk := make([]byte, 64*1024)
	for {
		n, err := f.Read(chunk)
		buf = append(buf, chunk[:n]...)
		if err != nil || n == 0 {
			return buf
		}
	}
}

// transcribeFile uploads the WAV file's PCM without its leading and trailing silence. A tap (under minRecording) or
// audio with nothing above trim_below_db sends nothing and transcribes to "" ("Nothing heard.").
func transcribeFile(ctx context.Context, client *gateway.Client, model, language, wav string, belowDB int) (string, error) {
	data, err := os.ReadFile(wav)
	if err != nil {
		return "", err
	}
	offset, length, ok := audio.Data(data)
	if !ok {
		return "", errors.New("Recording failed: not a WAV file")
	}
	pcm := data[offset : offset+length]
	start, end := audio.Trim(pcm, belowDB)
	if len(pcm) < minRecording || start == end {
		slog.Info("nothing to send", "bytes", len(pcm))
		return "", nil
	}
	ctx, cancel := context.WithTimeout(ctx, gateway.TranscribeTimeout) // a body past the socket buffers can stall
	defer cancel()
	return client.Transcribe(ctx, model, language, bytes.NewReader(pcm[start:end]), int64(end-start))
}
