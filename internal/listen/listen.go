// Package listen is `habla listen`: record, transcribe, transform, copy and optionally paste,
// coordinating through the runtime directory.
package listen

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/ackstorm/habla/internal/config"
	"github.com/ackstorm/habla/internal/desktop"
	"github.com/ackstorm/habla/internal/logs"
	"github.com/ackstorm/habla/internal/rundir"
)

// Self is the habla binary that --start runs in the background. ponytail: a var only so tests can point it at a
// link named habla (rundir.Alive checks that name).
var Self = os.Executable

func Main(args []string) int {
	fs := flag.NewFlagSet("habla listen", flag.ContinueOnError)
	paste := fs.Bool("paste", false, "also paste into the focused window")
	enter := fs.Bool("paste_enter", false, "also paste, then press Enter")
	start := fs.Bool("start", false, "start recording in the background (while busy: continue the dictation)")
	stop := fs.Bool("stop", false, "stop the running recording and transcribe")
	toggle := fs.Bool("toggle", false, "start, or stop if one is running")
	cancel := fs.Bool("cancel", false, "discard the recording in progress (while busy: the whole dictation)")
	retry := fs.Bool("retry", false, "rerun the pipeline on failed.wav")
	send := fs.Bool("send", false, "while busy: paste the dictation and press Enter when it is ready")
	status := fs.Bool("status", false, "print idle, recording, busy, transform, copying, pasting, copy_failed or failed")
	session := fs.Bool("session", false, "internal: the background recording that --start runs")
	providerFlag := fs.String("provider", "", "use the stt provider `NAME` for this run")
	transform = nil // Main runs more than once in tests
	fs.Func("transform", "use the [stt.transforms.*] `NAME` for this run (\"\": none)", func(s string) error {
		transform = &s
		return nil
	})
	logLevel := fs.String("log", "", "log at `LEVEL` (error, warn, info, debug) for this run")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	provider, pasteEnter = *providerFlag, *enter
	if err := logs.Setup("listen", *logLevel); err != nil {
		fmt.Fprintln(os.Stderr, "habla listen:", err)
		return 2
	}
	modes := 0
	for _, mode := range []*bool{start, stop, toggle, cancel, retry, send, status, session} {
		if *mode {
			modes++
		}
	}
	if modes > 1 || fs.NArg() > 0 {
		fmt.Fprintln(os.Stderr, "habla listen: use one of --start, --stop, --toggle, --cancel, --retry, --send, --status")
		return 2
	}
	switch {
	case *status:
		fmt.Println(rundir.ReadState())
		return 0
	case *stop:
		return signalRecording(syscall.SIGUSR1)
	case *cancel:
		return signalRecording(syscall.SIGTERM)
	case *retry:
		return retryFailed(*paste)
	case *send:
		return markSend()
	case *toggle:
		switch {
		case rundir.RunningPID(rundir.Listen) == 0:
			return startBackground(*paste)
		case rundir.ReadState() == "recording":
			return signalRecording(syscall.SIGUSR1)
		}
		return startOrContinue(*paste) // busy: record another part of the dictation
	case *start:
		return startOrContinue(*paste)
	case *session:
		syscall.CloseOnExec(3) // the recorder must not keep the --start parent's pipe open
		return runSession(nil, os.NewFile(3, "ready"), *paste)
	}
	if !desktop.Terminal(os.Stdin) {
		fmt.Fprintln(os.Stderr, "No terminal: use --start / --stop")
		return 2
	}
	return runSession(os.Stdin, nil, *paste)
}

// provider is --provider: the stt provider for this run, passed on to the background session. ponytail: a package
// var set by Main, instead of a parameter through every mode.
var provider string

// pasteEnter is --paste_enter, like provider.
var pasteEnter bool

// transform is --transform, like provider; nil: the provider's own.
var transform *string

// startBackground runs `habla listen --session` detached (Go cannot fork) and returns once it is recording, or has
// failed to start.
func startBackground(paste bool) int {
	self, err := Self()
	if err != nil {
		fmt.Fprintln(os.Stderr, "habla listen:", err)
		return 1
	}
	r, w, err := os.Pipe()
	if err != nil {
		fmt.Fprintln(os.Stderr, "habla listen:", err)
		return 1
	}
	args := []string{"listen", "--session"}
	if paste {
		args = append(args, "--paste")
	}
	if pasteEnter {
		args = append(args, "--paste_enter")
	}
	if provider != "" {
		args = append(args, "--provider", provider)
	}
	if transform != nil {
		args = append(args, "--transform", *transform)
	}
	cmd := exec.Command(self, args...) // stdin, stdout and stderr: /dev/null
	cmd.Args[0] = "habla"              // rundir.Alive only accepts a process named habla
	cmd.ExtraFiles = []*os.File{w}     // fd 3 in the child
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	err = cmd.Start()
	w.Close()
	if err != nil {
		r.Close()
		fmt.Fprintln(os.Stderr, "habla listen:", err)
		return 1
	}
	message, _ := io.ReadAll(r)
	r.Close()
	cmd.Process.Release()
	if string(message) == "recording" {
		return 0
	}
	if len(message) == 0 {
		message = []byte("habla listen: the recording process died")
	}
	fmt.Fprintln(os.Stderr, string(message))
	return 1
}

// startOrContinue asks a busy session to record another part of its dictation (SIGUSR2) and returns once it records.
// A session still recording is given time to reach busy: a quick re-press can land between the release's --stop and
// the session stopping its recorder. Otherwise, or once that session has delivered and left, it starts a new one.
func startOrContinue(paste bool) int {
	pid := rundir.RunningPID(rundir.Listen)
	if pid == 0 {
		return startBackground(paste)
	}
	asked := false
	for range 100 { // 2 s
		state := rundir.ReadState()
		switch {
		case rundir.RunningPID(rundir.Listen) != pid:
			return startBackground(paste) // it delivered (or failed) first
		case asked && state == "recording":
			return 0
		case !asked && rundir.Processing(state):
			if syscall.Kill(pid, syscall.SIGUSR2) != nil {
				return startBackground(paste)
			}
			asked = true
		}
		time.Sleep(20 * time.Millisecond)
	}
	if asked {
		fmt.Fprintln(os.Stderr, "Habla is busy processing")
	} else {
		fmt.Fprintln(os.Stderr, "Already recording")
	}
	return 1
}

// signalRecording sends sig to the session: SIGUSR1 (--stop) while recording; SIGTERM (--cancel) while recording, or
// while busy, which discards the whole dictation.
func signalRecording(sig syscall.Signal) int {
	pid, state := rundir.RunningPID(rundir.Listen), rundir.ReadState()
	if pid == 0 || !(state == "recording" || rundir.Processing(state) && sig == syscall.SIGTERM) {
		fmt.Fprintln(os.Stderr, "Not recording")
		return 1
	}
	if err := syscall.Kill(pid, sig); err != nil {
		fmt.Fprintln(os.Stderr, "habla listen:", err)
		return 1
	}
	return 0
}

// markSend is --send (the tray's Enter while busy, or with the hotkey held: that --stop comes first, so the session
// may still be recording). ponytail: a mark that lands after deliver has looked is unused; the next session removes
// it.
func markSend() int {
	state := rundir.ReadState()
	if rundir.RunningPID(rundir.Listen) == 0 || state != "recording" && !rundir.Processing(state) {
		fmt.Fprintln(os.Stderr, "Not busy")
		return 1
	}
	if err := os.WriteFile(rundir.SendMark(), nil, 0o600); err != nil {
		fmt.Fprintln(os.Stderr, "habla listen:", err)
		return 1
	}
	return 0
}

func retryFailed(paste bool) int {
	signal.Ignore(syscall.SIGUSR2) // busy invites another part; a retry has none (--cancel ends it, failed.wav kept)
	cfg, err := loadConfig(true, paste)
	if err != nil {
		return 1
	}
	ok, err := rundir.Claim(rundir.Listen, "")
	if err != nil || !ok {
		fmt.Fprintln(os.Stderr, "Habla is busy")
		return 1
	}
	defer rundir.Release(rundir.Listen)
	os.Remove(rundir.SendMark()) // a mark left by an earlier dictation
	failed := filepath.Join(rundir.Dir(), "failed.wav")
	if _, err := os.Stat(failed); err != nil {
		fmt.Fprintln(os.Stderr, "Nothing to retry")
		return 1
	}
	d := &dictation{cfg: cfg, desktopNotify: true}
	d.add(failed, startUpload(cfg, failed))
	code, _ := d.finish()
	return code
}

// loadConfig reports an invalid config itself: the caller only exits 1.
func loadConfig(desktopNotify, paste bool) (*config.Config, error) {
	cfg, err := config.Load()
	if err != nil {
		desktop.ConfigError(err, desktopNotify)
		return nil, err
	}
	if cfg.STT == nil {
		err := fmt.Errorf("%s: no [stt.providers.<name>] section", config.Path())
		desktop.ConfigError(err, desktopNotify)
		return nil, err
	}
	if provider != "" {
		if err := cfg.Use("stt", provider); err != nil {
			desktop.ConfigError(err, desktopNotify)
			return nil, err
		}
	}
	if transform != nil {
		if err := cfg.UseTransform(*transform); err != nil {
			desktop.ConfigError(err, desktopNotify)
			return nil, err
		}
	}
	logs.SetLevel(cfg.LogLevel)
	cfg.Paste, cfg.PasteEnter = paste || pasteEnter, pasteEnter
	return cfg, nil
}
