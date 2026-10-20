package audio

import (
	"bytes"
	"encoding/binary"
	"log/slog"
	"math"
)

const (
	Rate       = 16000 // Hz, mono, s16
	FrameBytes = 640   // 20 ms
	PadFrames  = 10    // 200 ms kept on each side of the speech
)

// Header is a 44-byte 16 kHz mono s16 WAV header for size bytes of PCM. size < 0 gives the streaming sizes
// (0xFFFFFFFF), which the gateway accepts on a chunked upload; a 0 size is rejected (probed 2026-10-16).
func Header(size int64) []byte { return HeaderAt(Rate, size) }

// HeaderAt is Header for a mono s16 stream at rate Hz.
func HeaderAt(rate, size int64) []byte {
	riff, data := uint32(0xFFFFFFFF), uint32(0xFFFFFFFF)
	if size >= 0 {
		riff, data = uint32(36+size), uint32(size)
	}
	le := binary.LittleEndian
	h := append([]byte("RIFF"), le.AppendUint32(nil, riff)...)
	h = append(h, "WAVEfmt "...)
	h = le.AppendUint32(h, 16)
	h = le.AppendUint16(h, 1) // PCM
	h = le.AppendUint16(h, 1) // mono
	h = le.AppendUint32(h, uint32(rate))
	h = le.AppendUint32(h, uint32(rate*2))
	h = le.AppendUint16(h, 2)
	h = le.AppendUint16(h, 16)
	h = append(h, "data"...)
	return le.AppendUint32(h, data)
}

// Data finds the PCM in a WAV file's bytes. length is the data chunk's size when the file holds that much (the
// recorder has finalized the header), else everything after offset (sizes are written only on stop). ok is false
// until the data chunk header is there.
func Data(wav []byte) (offset, length int, ok bool) {
	if len(wav) < 12 || string(wav[:4]) != "RIFF" || string(wav[8:12]) != "WAVE" {
		return 0, 0, false
	}
	for pos := 12; pos+8 <= len(wav); {
		size := int(binary.LittleEndian.Uint32(wav[pos+4 : pos+8]))
		if string(wav[pos:pos+4]) == "data" {
			offset, length = pos+8, len(wav)-(pos+8)
			if size > 0 && size <= length {
				length = size
			}
			return offset, length &^ 1, true
		}
		pos += 8 + size + size&1
	}
	return 0, 0, false
}

// Voiced reports whether a frame's RMS level reaches belowDB dBFS.
func Voiced(frame []byte, belowDB int) bool {
	n := len(frame) / 2
	var sum float64
	for i := 0; i < n; i++ {
		s := float64(int16(binary.LittleEndian.Uint16(frame[2*i:])))
		sum += s * s
	}
	if sum == 0 {
		return false
	}
	return 20*math.Log10(math.Sqrt(sum/float64(n))/32768) >= float64(belowDB)
}

// Trim returns the bounds of pcm without its leading and trailing silence, keeping PadFrames on each side.
// belowDB 0 turns it off, and all-silent audio is returned whole (the gateway decides "Nothing heard").
// ponytail: belowDB is the calibration knob for quiet microphones (trim_below_db).
func Trim(pcm []byte, belowDB int) (start, end int) {
	first, last := -1, -1
	if belowDB != 0 {
		for i := 0; (i+1)*FrameBytes <= len(pcm); i++ {
			if Voiced(pcm[i*FrameBytes:(i+1)*FrameBytes], belowDB) {
				if first < 0 {
					first = i
				}
				last = i
			}
		}
	}
	if first < 0 {
		if belowDB != 0 {
			slog.Warn("trim: nothing above trim_below_db, sending the audio whole", "db", belowDB, "bytes", len(pcm))
		}
		return 0, len(pcm)
	}
	start, end = max(0, first-PadFrames)*FrameBytes, min(len(pcm), (last+1+PadFrames)*FrameBytes)
	slog.Debug("trim", "start", start, "end", end, "of", len(pcm))
	return start, end
}

// Unwrap returns the WAV inside a WAV's data chunk, or wav itself. Gemini TTS (gemini-3.8-flash-tts) already
// returns a whole WAV, and LiteLLM wraps it in a second header (BerriAI/litellm#44541), so players would play the
// inner header (a click) and Gemini's trailing C2PA chunk (~126 ms of noise) as audio.
func Unwrap(wav []byte) []byte {
	if offset, length, ok := Data(wav); ok {
		if inner := wav[offset : offset+length]; bytes.HasPrefix(inner, []byte("RIFF")) {
			if _, _, ok := Data(inner); ok {
				return inner
			}
		}
	}
	return wav
}
