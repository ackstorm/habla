package audio_test

import (
	"bytes"
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ackstorm/habla/internal/audio"
	"github.com/ackstorm/habla/internal/testenv"
)

func TestMain(m *testing.M) {
	testenv.Fake()
	os.Exit(m.Run())
}

func TestRecorderPrefersPwRecord(t *testing.T) {
	e := testenv.Setup(t)
	args, err := audio.RecorderCommand("x.wav")
	if err != nil || !slices.Equal(args, []string{"pw-record", "--rate", "16000", "--channels", "1", "--format", "s16", "x.wav"}) {
		t.Fatal(args, err)
	}
	e.Remove("pw-record")
	args, _ = audio.RecorderCommand("x.wav")
	if !slices.Equal(args, []string{"parecord", "--rate=16000", "--channels=1", "--format=s16le", "--file-format=wav", "x.wav"}) {
		t.Fatal(args)
	}
	e.Remove("parecord")
	if _, err := audio.RecorderCommand("x.wav"); err == nil || !strings.Contains(err.Error(), "no recorder") {
		t.Fatal(err)
	}
}

func TestRecorderStopsOnSIGINTAndKeepsTheAudio(t *testing.T) {
	e := testenv.Setup(t)
	wav := filepath.Join(e.Dir, "current.wav")
	rec, err := audio.Start(wav)
	if err != nil {
		t.Fatal(err)
	}
	testenv.WaitFor(t, "the whole recording", func() bool { return len(e.Lines("recorded")) > 0 })
	rec.Stop()
	if err := rec.StartError(); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(wav)
	offset, length, ok := audio.Data(data)
	if !ok || !bytes.Equal(data[offset:offset+length], testenv.HelloPCM(t)) {
		t.Fatal("the WAV does not hold the recorded audio")
	}
}

func TestRecorderThatExitsAtOnceReportsItsLastStderrLine(t *testing.T) {
	e := testenv.Setup(t)
	t.Setenv("HABLA_FAKE_FAIL", "pw-record")
	rec, err := audio.Start(filepath.Join(e.Dir, "current.wav"))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-rec.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("the failing recorder did not exit")
	}
	if err := rec.StartError(); err == nil || err.Error() != "no microphone" {
		t.Fatal(err)
	}
}

func TestHeaderAndData(t *testing.T) {
	wav := append(audio.Header(4), 1, 2, 3, 4)
	if offset, length, ok := audio.Data(wav); !ok || offset != 44 || length != 4 {
		t.Fatal(offset, length, ok)
	}
	stream := append(audio.Header(-1), 1, 2, 3, 4, 5, 6)
	if !bytes.Equal(stream[40:44], []byte{0xff, 0xff, 0xff, 0xff}) {
		t.Fatal("streaming data size is not 0xFFFFFFFF")
	}
	if _, length, _ := audio.Data(stream); length != 6 {
		t.Fatal(length)
	}
	growing := append(audio.Header(0), 1, 2, 3) // size 0 while recording: everything so far, whole samples only
	if _, length, _ := audio.Data(growing); length != 2 {
		t.Fatal(length)
	}
}

func TestDataSkipsOtherChunksAndWaitsForTheDataHeader(t *testing.T) {
	h := audio.Header(2)
	list := append([]byte("LIST"), 3, 0, 0, 0, 'a', 'b', 'c', 0) // odd size: padded to even
	wav := append(append(append([]byte{}, h[:36]...), list...), append(h[36:], 7, 8)...)
	if offset, length, ok := audio.Data(wav); !ok || offset != 36+12+8 || length != 2 {
		t.Fatal(offset, length, ok)
	}
	if _, _, ok := audio.Data(h[:40]); ok {
		t.Fatal("found data in a truncated header")
	}
}

// pcm is ms milliseconds of a 440 Hz tone at amplitude (0 = silence).
func pcm(ms int, amplitude float64) []byte {
	var b []byte
	for i := 0; i < ms*audio.Rate/1000; i++ {
		b = binary.LittleEndian.AppendUint16(b, uint16(int16(amplitude*math.Sin(2*math.Pi*440*float64(i)/audio.Rate))))
	}
	return b
}

func TestTrim(t *testing.T) {
	speech := slices.Concat(pcm(1000, 0), pcm(500, 8000), pcm(1000, 50)) // amplitude 50 ≈ -59 dBFS: silence
	start, end := audio.Trim(speech, -45)
	if start != (50-audio.PadFrames)*audio.FrameBytes || end != (50+25+audio.PadFrames)*audio.FrameBytes {
		t.Fatal(start, end)
	}
	if start, end := audio.Trim(speech, 0); start != 0 || end != len(speech) {
		t.Fatal("trimming off still trimmed", start, end)
	}
	silent := pcm(1000, 0)
	if start, end := audio.Trim(silent, -45); start != 0 || end != len(silent) {
		t.Fatal("all-silent audio must be kept whole", start, end)
	}
	if start, end := audio.Trim(testenv.HelloPCM(t), -45); start >= end {
		t.Fatal("the hello clip trimmed away", start, end)
	}
}

func TestUnwrapReturnsTheInnerWAV(t *testing.T) {
	pcm := []byte{1, 0, 2, 0}
	inner := append(audio.Header(int64(len(pcm))), pcm...)
	inner = append(inner, binary.LittleEndian.AppendUint32([]byte("C2PA"), 4)...)
	inner = append(inner, "jumb"...)
	outer := append(audio.Header(int64(len(inner))), inner...)
	if got := audio.Unwrap(outer); !bytes.Equal(got, inner) {
		t.Fatalf("%x", got)
	}
	if got := audio.Unwrap(inner); !bytes.Equal(got, inner) {
		t.Fatalf("%x", got)
	}
}
