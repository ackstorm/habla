package testenv

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/ackstorm/habla/internal/audio"
)

func init() {
	fakes["pw-record"] = recorder
	fakes["parecord"] = recorder
}

// helloLevels is the RMS level in dBFS of each 20 ms frame of Hello; frames not listed are digital silence.
// The shape reproduces the recorded "Hello world." this clip replaced (so the trim and split tests keep their numbers):
// a click in frame 0, speech in frames 12-50 (trimmed at -45 dB), a quiet gap, a burst in frames 66-72, then silence
// from frame 73 (1.46 s) to the end.
func helloLevels(frame int) (db float64, ok bool) {
	switch {
	case frame == 0:
		return -21, true
	case frame < 12:
		return -75, true
	case frame < 14:
		return -35, true
	case frame < 46:
		return -10, true
	case frame < 51:
		return -28, true
	case frame < 66:
		return -75, true
	case frame < 73:
		return -7, true
	}
	return 0, false
}

// HelloWAV is a generated 2.0 s 16 kHz mono s16 clip standing in for a spoken "Hello world.": a 200 Hz tone
// whose level follows helloLevels.
func HelloWAV() []byte {
	pcm := make([]byte, 100*audio.FrameBytes)
	for i := 0; i < len(pcm)/2; i++ {
		db, ok := helloLevels(i * 2 / audio.FrameBytes)
		if !ok {
			continue
		}
		amp := math.Pow(10, db/20) * 32768 * math.Sqrt2 // a sine's peak is √2 times its RMS
		v := int16(math.Round(amp * math.Sin(2*math.Pi*200*float64(i)/audio.Rate)))
		binary.LittleEndian.PutUint16(pcm[2*i:], uint16(v))
	}
	return append(audio.Header(int64(len(pcm))), pcm...)
}

// HelloPCM is the PCM in HelloWAV.
func HelloPCM(t testing.TB) []byte {
	t.Helper()
	wav := HelloWAV()
	offset, length, ok := audio.Data(wav)
	if !ok {
		t.Fatal("HelloWAV has no data chunk")
	}
	return wav[offset : offset+length]
}

// recorder pretends to be pw-record or parecord. It writes HelloWAV's PCM ($HABLA_FAKE_RECORDING's, when set) to the WAV
// named by its last argument, 100 ms of audio every 10 ms, behind a header with size 0 (as a streaming writer does),
// and appends a line to "recorded" once all of it is written. On SIGINT it writes the final sizes and exits 0.
func recorder(args []string) int {
	name := filepath.Base(os.Args[0])
	if failing(name) {
		fmt.Fprintf(os.Stderr, "%s: connecting\nno microphone\n", name)
		return 1
	}
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)
	data := HelloWAV()
	if p := os.Getenv("HABLA_FAKE_RECORDING"); p != "" {
		var err error
		if data, err = os.ReadFile(p); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	offset, length, _ := audio.Data(data)
	pcm := data[offset : offset+length]
	if slices.Contains(args, "--raw") {
		for {
			for pos := 0; pos < len(pcm); pos += 960 {
				if _, err := os.Stdout.Write(pcm[pos:min(pos+960, len(pcm))]); err != nil {
					return 1
				}
				time.Sleep(20 * time.Millisecond)
			}
		}
	}
	f, err := os.Create(args[len(args)-1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer f.Close()
	f.Write(audio.Header(0))
	written := 0
	for written < len(pcm) {
		n := min(3200, len(pcm)-written)
		f.Write(pcm[written : written+n])
		written += n
		select {
		case <-interrupt:
			return finish(f, written)
		case <-time.After(10 * time.Millisecond):
		}
	}
	appendLine("recorded", "all")
	<-interrupt
	return finish(f, written)
}

func finish(f *os.File, written int) int {
	if _, err := f.WriteAt(audio.Header(int64(written)), 0); err != nil {
		return 1
	}
	return 0
}

// Speech16k converts a mono s16 WAV at any rate (a TTS reply, usually 24 kHz) to 16 kHz PCM by linear
// interpolation: enough for live tests that feed a synthesized "Hello world." to a real STT.
func Speech16k(wav []byte) ([]byte, error) {
	offset, length, ok := audio.Data(wav)
	if !ok || len(wav) < 28 {
		return nil, fmt.Errorf("speech is not a WAV with a data chunk")
	}
	rate := int(binary.LittleEndian.Uint32(wav[24:]))
	in := wav[offset : offset+length]
	n := len(in) / 2
	out := make([]byte, 2*(n*audio.Rate/rate))
	sample := func(i int) int { return int(int16(binary.LittleEndian.Uint16(in[2*min(i, n-1):]))) }
	for i := 0; i < len(out)/2; i++ {
		pos := i * rate
		a, frac := pos/audio.Rate, pos%audio.Rate
		x, y := sample(a), sample(a+1)
		binary.LittleEndian.PutUint16(out[2*i:], uint16(int16(x+(y-x)*frac/audio.Rate)))
	}
	return out, nil
}
