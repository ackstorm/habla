package listen

import (
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/ackstorm/habla/internal/audio"
	"github.com/ackstorm/habla/internal/config"
	"github.com/ackstorm/habla/internal/desktop"
	"github.com/ackstorm/habla/internal/rundir"
	"golang.org/x/sys/unix"
)

// runSession is one dictation: claim listen.pid, record a part, transcribe it and deliver the text. In the
// background, SIGUSR2 while busy records another part of the same dictation. keys is the terminal of an interactive
// run (nil in the background); ready is the pipe that tells the --start parent whether recording began (nil when
// interactive).
func runSession(keys, ready *os.File, paste bool) int {
	desktopNotify := keys == nil
	signals := make(chan os.Signal, 2) // before the claim: a --stop may arrive as soon as the state is published
	signal.Notify(signals, syscall.SIGUSR1, syscall.SIGTERM, os.Interrupt)
	defer signal.Stop(signals)
	var more chan os.Signal
	if keys == nil {
		more = make(chan os.Signal, 1)
		// ponytail: never stopped: a SIGUSR2 after the delivery is dropped instead of killing the exiting process
		signal.Notify(more, syscall.SIGUSR2)
	} else {
		signal.Ignore(syscall.SIGUSR2) // an interactive run records one part
	}
	cfg, err := loadConfig(desktopNotify, paste)
	if err != nil {
		tell(ready, "Invalid config: "+err.Error())
		return 1
	}
	ok, err := rundir.Claim(rundir.Listen, "recording")
	if err != nil || !ok {
		message := "Already recording"
		if err != nil {
			message = "Habla runtime directory: " + err.Error()
		} else if rundir.Processing(rundir.ReadState()) {
			message = "Habla is busy processing"
		}
		fmt.Fprintln(os.Stderr, message)
		tell(ready, message)
		return 1
	}
	defer rundir.Release(rundir.Listen)
	d := &dictation{cfg: cfg, desktopNotify: desktopNotify, signals: signals, more: more}
	current := filepath.Join(rundir.Dir(), "current.wav")
	os.Remove(current) // a file left by a killed session must not be read as this recording
	for {
		if code, ok := d.record(current, keys, ready); !ok {
			return code
		}
		ready = nil // told once
		code, again := d.finish()
		if !again {
			return code
		}
		rundir.WriteState("recording")
	}
}

// record records one part into current.wav (the tray's pill reads it) and starts its transcription; the stopped
// recording becomes a file of its own, since the next part reuses current.wav. false: the session ends with code
// (its first part was discarded or failed). A later part that is discarded or fails leaves the earlier ones.
func (d *dictation) record(current string, keys, ready *os.File) (code int, ok bool) {
	later := len(d.parts) > 0
	select {
	case <-d.more: // a second SIGUSR2 for this same part
	default:
	}
	discard := func() (int, bool) {
		os.Remove(current)
		if !later {
			rundir.WriteState("")
		}
		return 1, later
	}
	rec, err := audio.Start(current)
	if err != nil {
		recordingFailed(err, ready, d.desktopNotify)
		return discard()
	}
	tell(ready, "recording") // at once: a recorder that fails to start is seen by waitForStop
	started := time.Now()
	if keys != nil {
		fmt.Fprintln(os.Stderr, "Recording… Enter stops, Esc discards.")
	}
	u := startUpload(d.cfg, current)
	u.streamWhileRecording()
	outcome := waitForStop(d.cfg, rec, keys, d.signals)
	rec.Stop()
	if err := rec.StartError(); err != nil {
		u.abort()
		recordingFailed(err, nil, d.desktopNotify)
		return discard()
	}
	var size int64
	if info, err := os.Stat(current); err == nil {
		size = info.Size()
	}
	slog.Info("recording stopped", "part", len(d.parts)+1, "outcome", outcome,
		"seconds", time.Since(started).Round(100*time.Millisecond).Seconds(), "bytes", size)
	if outcome == "cancel" {
		u.abort()
		fmt.Fprintln(os.Stderr, "Discarded.")
		return discard()
	}
	wav := filepath.Join(rundir.Dir(), fmt.Sprintf("part-%d-%d.wav", os.Getpid(), len(d.parts)+1))
	if err := os.Rename(current, wav); err != nil {
		u.abort()
		desktop.Notify("Recording failed: no audio was recorded", "", d.desktopNotify)
		return discard()
	}
	d.add(wav, u)
	return 0, true
}

func recordingFailed(err error, ready *os.File, desktopNotify bool) {
	slog.Error("recording failed", "err", err)
	desktop.Notify("Recording failed: "+err.Error(), "", desktopNotify)
	tell(ready, "Recording failed: "+err.Error())
}

// tell sends the --start parent "recording" or why not, once. Interactive runs have no parent (ready is nil).
func tell(ready *os.File, message string) {
	if ready != nil {
		ready.WriteString(message)
		ready.Close()
	}
}

// waitForStop records until Enter, SIGUSR1, max_seconds or the recorder exiting ("stop"), or Esc, SIGTERM or
// Ctrl+C ("cancel"). Signals win over the other events when several are pending.
func waitForStop(cfg *config.Config, rec *audio.Recorder, keys *os.File, signals <-chan os.Signal) string {
	var pressed <-chan byte
	if keys != nil {
		if restore, err := cbreak(keys); err == nil {
			defer restore()
		}
		pressed = readKeys(keys)
	}
	deadline := time.NewTimer(time.Duration(cfg.MaxSeconds) * time.Second)
	defer deadline.Stop()
	asked := func(s os.Signal) string {
		if s == syscall.SIGUSR1 {
			return "stop"
		}
		return "cancel"
	}
	for {
		select {
		case s := <-signals:
			return asked(s)
		default:
		}
		select {
		case s := <-signals:
			return asked(s)
		case <-rec.Done(): // ponytail: a recorder that dies mid-recording counts as stop; the pipeline sees what it wrote
			return "stop"
		case <-deadline.C:
			return "stop"
		case k := <-pressed:
			switch k {
			case '\n', '\r':
				return "stop"
			case 0x1b: // ponytail: arrow keys start with Esc too and also discard
				return "cancel"
			}
		}
	}
}

// readKeys delivers the terminal's bytes one by one.
// ponytail: the reader goroutine outlives the session; the process exits right after.
func readKeys(f *os.File) <-chan byte {
	c := make(chan byte)
	go func() {
		b := make([]byte, 1)
		for {
			if n, err := f.Read(b); err != nil {
				return
			} else if n == 1 {
				c <- b[0]
			}
		}
	}()
	return c
}

// cbreak gives one key at a time without echo; Ctrl+C still raises SIGINT. Typed-ahead keys are kept (TCSANOW).
func cbreak(f *os.File) (restore func(), err error) {
	fd := int(f.Fd())
	saved, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return nil, err
	}
	raw := *saved
	raw.Lflag &^= unix.ICANON | unix.ECHO
	raw.Cc[unix.VMIN], raw.Cc[unix.VTIME] = 1, 0
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, &raw); err != nil {
		return nil, err
	}
	return func() { unix.IoctlSetTermios(fd, unix.TCSETSW, saved) }, nil
}
