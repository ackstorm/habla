// Package audio records 16 kHz mono s16 WAV with pw-record or parecord, and handles WAV headers and silence
// trimming.
package audio

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"
)

// StartupWindow: a recorder that exits by itself this fast failed to start (no microphone, no sound server).
const StartupWindow = 300 * time.Millisecond

// RecorderCommand is pw-record (PipeWire), else parecord (PulseAudio), writing a 16 kHz mono s16 WAV to wav.
func RecorderCommand(wav string) ([]string, error) {
	if _, err := exec.LookPath("pw-record"); err == nil {
		return []string{"pw-record", "--rate", "16000", "--channels", "1", "--format", "s16", wav}, nil
	}
	if _, err := exec.LookPath("parecord"); err == nil {
		return []string{"parecord", "--rate=16000", "--channels=1", "--format=s16le", "--file-format=wav", wav}, nil
	}
	return nil, errors.New("no recorder: install pipewire-bin (pw-record) or pulseaudio-utils (parecord)")
}

// Recorder is a running recorder process.
type Recorder struct {
	cmd      *exec.Cmd
	stderr   *os.File
	done     chan struct{}
	stopping atomic.Bool
	early    bool // set before done is closed: exited by itself within StartupWindow
}

// Start runs the recorder and returns at once; it does not wait to see whether it keeps running (see StartError).
func Start(wav string) (*Recorder, error) {
	args, err := RecorderCommand(wav)
	if err != nil {
		return nil, err
	}
	stderr, err := os.CreateTemp("", "habla-recorder-*") // a file, not a pipe: an undrained pipe would stall it
	if err != nil {
		return nil, err
	}
	os.Remove(stderr.Name())
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		stderr.Close()
		return nil, err
	}
	slog.Info("recording", "recorder", args[0])
	r := &Recorder{cmd: cmd, stderr: stderr, done: make(chan struct{})}
	started := time.Now()
	go func() {
		cmd.Wait()
		r.early = !r.stopping.Load() && time.Since(started) < StartupWindow
		close(r.done)
	}()
	return r, nil
}

// Done is closed once the recorder has exited.
func (r *Recorder) Done() <-chan struct{} { return r.done }

// Stop ends the recording with SIGINT, on which both recorders finish the WAV header, and waits for the exit
// (SIGKILL after 5 s).
func (r *Recorder) Stop() {
	r.stopping.Store(true)
	select {
	case <-r.done:
		return
	default:
	}
	r.cmd.Process.Signal(os.Interrupt)
	select {
	case <-r.done:
	case <-time.After(5 * time.Second):
		r.cmd.Process.Kill()
		<-r.done
	}
}

// StartError is why a recorder that exited by itself within StartupWindow failed: its last stderr line. It is nil
// otherwise. Call it after Done is closed or Stop has returned.
func (r *Recorder) StartError() error {
	if !r.early {
		return nil
	}
	r.stderr.Seek(0, io.SeekStart)
	out, _ := io.ReadAll(r.stderr)
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if last := strings.TrimSpace(lines[len(lines)-1]); last != "" {
		return errors.New(last)
	}
	return fmt.Errorf("%s exited with status %d", r.cmd.Args[0], r.cmd.ProcessState.ExitCode())
}
