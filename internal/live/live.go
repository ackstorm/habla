// Package live is the foreground realtime speech experiment.
package live

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/ackstorm/habla/internal/config"
	"github.com/ackstorm/habla/internal/desktop"
	"github.com/ackstorm/habla/internal/gateway"
	"github.com/ackstorm/habla/internal/logs"
	"github.com/ackstorm/habla/internal/rundir"
	"golang.org/x/sys/unix"
)

func Main(args []string) (code int) {
	fs := flag.NewFlagSet("habla live", flag.ContinueOnError)
	contextFile := fs.String("context", "", "text `FILE` to discuss (sent to the realtime service)")
	noOutput := fs.Bool("no-output", false, "do not save a transcript (terminal output remains enabled)")
	outputFile := fs.String("output-file", "", "save the transcript to `FILE` instead of the default timestamped file")
	provider := fs.String("provider", "", "use the live provider `NAME` for this run")
	logLevel := fs.String("log", "", "log at `LEVEL` for this run")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 || (*noOutput && *outputFile != "") {
		fmt.Fprintln(os.Stderr, "usage: habla live [--context FILE] [--no-output | --output-file FILE] [--provider NAME] [--log LEVEL]")
		return 2
	}
	if err := logs.Setup("live", *logLevel); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	background, err := readContext(*contextFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "habla live:", err)
		return 1
	}
	cfg, err := config.Load()
	if err != nil {
		desktop.ConfigError(err, false)
		return 1
	}
	logs.SetLevel(cfg.LogLevel)
	if cfg.Live == nil {
		fmt.Fprintln(os.Stderr, "Live is not configured: add a [live.providers.<name>] section to config.toml")
		return 1
	}
	if *provider != "" {
		if err := cfg.Use("live", *provider); err != nil {
			fmt.Fprintln(os.Stderr, "habla live:", err)
			return 1
		}
	}
	style := cfg.LiveDelivery()
	if (cfg.Live.Style != "" || cfg.Live.Voice != "") && cfg.Live.InstructionsMode == "" {
		slog.Warn("Live voice and style are not applied: set instructions_mode in the live provider")
	}
	if !desktop.Terminal(os.Stdin) {
		fmt.Fprintln(os.Stderr, "habla live requires a terminal; Ctrl+C ends the session")
		return 2
	}
	if cfg.MaxSeconds <= 0 {
		fmt.Fprintln(os.Stderr, "habla live: general.max_seconds must be positive")
		return 1
	}
	recorder, err := audioCommand(true)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	player, err := audioCommand(false)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	lock, err := os.OpenFile(filepath.Join(rundir.Dir(), "live.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Could not lock live session")
		return 1
	}
	defer lock.Close()
	if unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB) != nil {
		fmt.Fprintln(os.Stderr, "A live session is already running")
		return 1
	}
	var output io.Writer = os.Stdout
	if !*noOutput {
		file, err := openTranscript(*outputFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, "Could not create transcript file:", err)
			return 1
		}
		defer func() {
			if err := file.Close(); err != nil {
				fmt.Fprintln(os.Stderr, "Could not close transcript file")
				code = 1
			}
		}()
		output = io.MultiWriter(os.Stdout, file)
		fmt.Fprintln(os.Stderr, "Transcript:", file.Name())
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	session, end := context.WithTimeout(ctx, time.Duration(cfg.MaxSeconds)*time.Second)
	defer end()
	key, err := gateway.Credential(session, cfg.Live.APIKey)
	if err != nil {
		if session.Err() != nil {
			return 0
		}
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Fprintln(os.Stderr, "Connecting…")
	c, opening, err := connect(session, *cfg.Live, key, background, style)
	if err == nil {
		defer c.Close()
		recordCtx, stop := context.WithCancel(session)
		defer stop()
		cmd := audioProcess(recordCtx, recorder)
		mic, pipeErr := cmd.StdoutPipe()
		if pipeErr != nil {
			err = errors.New("Could not open microphone stream")
		} else if startErr := cmd.Start(); startErr != nil {
			mic.Close()
			err = errors.New("Could not start microphone capture")
		} else {
			slog.Info("live started")
			err = converse(session, c, mic, func(ctx context.Context, pcm io.Reader) error {
				play := audioProcess(ctx, player)
				play.Stdin = pcm
				return play.Run()
			}, opening, output, os.Stderr)
			stop()
			mic.Close()
			cmd.Wait()
			slog.Info("live ended")
		}
	}
	if session.Err() != nil {
		if ctx.Err() == nil {
			fmt.Fprintln(os.Stderr, "Live session reached general.max_seconds.")
		}
		return 0
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "habla live:", err)
		return 1
	}
	return 0
}

func readContext(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	f, err := os.Open(path)
	if err != nil {
		return "", errors.New("Could not read context file")
	}
	defer f.Close()
	const limit = 1 << 20
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return "", errors.New("Could not read context file")
	}
	if len(data) > limit {
		return "", errors.New("Context file exceeds 1 MiB")
	}
	if !utf8.Valid(data) {
		return "", errors.New("Context file must be UTF-8 text")
	}
	return string(data), nil
}

func audioCommand(record bool) ([]string, error) {
	pw, pa, kind := "pw-play", "paplay", "player"
	if record {
		pw, pa, kind = "pw-record", "parecord", "recorder"
	}
	if path, err := exec.LookPath(pw); err == nil {
		return []string{path, "--raw", "--rate=24000", "--channels=1", "--format=s16", "-"}, nil
	}
	if path, err := exec.LookPath(pa); err == nil {
		return []string{path, "--raw", "--rate=24000", "--channels=1", "--format=s16le"}, nil
	}
	return nil, fmt.Errorf("No %s: install pipewire-bin (%s) or pulseaudio-utils (%s)", kind, pw, pa)
}

func audioProcess(ctx context.Context, args []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 2 * time.Second
	return cmd
}
