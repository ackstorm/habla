// Package say is `habla say`: speak a text with the [tts] model, or write the audio to a file.
package say

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
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/ackstorm/habla/internal/config"
	"github.com/ackstorm/habla/internal/desktop"
	"github.com/ackstorm/habla/internal/gateway"
	"github.com/ackstorm/habla/internal/logs"
	"github.com/ackstorm/habla/internal/rundir"
	"github.com/ackstorm/habla/internal/stats"
)

const notConfigured = "TTS is not configured: add a [tts.providers.<name>] section to config.toml"

// Stdin is where the text comes from when there is no argument. ponytail: a var only so tests can replace it.
var Stdin io.Reader = os.Stdin

// started, when set, runs right after the player has been spawned. ponytail: a var only so tests can signal there.
var started func(pid int)

func Main(args []string) int {
	fs := flag.NewFlagSet("habla say", flag.ContinueOnError)
	stop := fs.Bool("stop", false, "stop the speech in progress")
	out := fs.String("out", "", "write the audio to `FILE` instead of playing it")
	provider := fs.String("provider", "", "use the tts provider `NAME` for this run")
	var style *string // set only when --style is given: "" means no style
	fs.Func("style", "use the [styles.*] `NAME` for this run (\"\": no style)", func(s string) error { style = &s; return nil })
	logLevel := fs.String("log", "", "log at `LEVEL` (error, warn, info, debug) for this run")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	text, haveText := "", false
	for fs.NArg() > 0 { // flags may follow the text, as with argparse
		if haveText {
			fmt.Fprintln(os.Stderr, "habla say: at most one text argument")
			return 2
		}
		text, haveText = fs.Arg(0), true
		if err := fs.Parse(fs.Args()[1:]); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return 0
			}
			return 2
		}
	}
	if err := logs.Setup("say", *logLevel); err != nil {
		fmt.Fprintln(os.Stderr, "habla say:", err)
		return 2
	}
	if *stop {
		pid := rundir.RunningPID(rundir.Say)
		if pid == 0 {
			desktop.Notify("Not speaking", "", false)
			return 1
		}
		if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
			desktop.Notify(err.Error(), "", false)
			return 1
		}
		return 0
	}
	cfg, err := config.Load()
	if err != nil {
		desktop.ConfigError(err, !desktop.Terminal(os.Stderr)) // a desktop shortcut has no terminal
		return 1
	}
	logs.SetLevel(cfg.LogLevel)
	if cfg.TTS == nil {
		desktop.Notify(notConfigured, "", false)
		return 1
	}
	if *provider != "" {
		if err := cfg.Use("tts", *provider); err != nil {
			fmt.Fprintln(os.Stderr, "habla say:", err)
			return 1
		}
	}
	if style != nil {
		if err := cfg.UseStyle("tts", *style); err != nil {
			fmt.Fprintln(os.Stderr, "habla say:", err)
			return 1
		}
	}
	if !haveText {
		if data, err := io.ReadAll(Stdin); err == nil {
			text = string(data)
		}
	}
	if text = strings.TrimSpace(text); text == "" {
		desktop.Notify("Nothing to say", "", false)
		return 1
	}
	terminal := desktop.Terminal(os.Stderr)
	if terminal {
		fmt.Fprintln(os.Stderr, "Processing...")
	}
	key, err := gateway.Credential(context.Background(), cfg.TTS.APIKey)
	if err != nil {
		desktop.Notify(err.Error(), "", false)
		return 1
	}
	client := gateway.New(cfg.TTS.Conn, key)
	voice, instructions, warning := cfg.TTS.Delivery()
	if warning != "" {
		slog.Warn(warning)
	}
	start := time.Now()
	var code int
	if *out != "" {
		code = write(client, cfg.TTS, voice, instructions, text, *out)
	} else {
		code = play(client, cfg.TTS, voice, instructions, text)
	}
	if calls := client.Calls(); len(calls) > 0 { // the gateway answered: the speech was paid for
		stats.Append(stats.Record{Time: start, Kind: "say", Chars: utf8.RuneCountInString(text),
			Words: len(strings.Fields(text)), Calls: stats.Calls(calls, cfg.Prices)})
	}
	if terminal && code == 0 {
		fmt.Fprintln(os.Stderr, "Done")
	}
	return code
}

func write(client *gateway.Client, tts *config.TTS, voice, instructions, text, path string) int {
	ctx := context.Background()
	audio, err := client.Speak(ctx, tts, voice, instructions, text)
	if err == nil {
		err = os.WriteFile(path, audio, 0o644)
	}
	if err != nil {
		slog.Error("say failed", "err", err)
		desktop.Notify(err.Error(), "", false)
		return 1
	}
	return 0
}

// play fetches and plays the audio while holding say.pid, so `--stop` (SIGTERM) can end it at any point.
func play(client *gateway.Client, tts *config.TTS, voice, instructions, text string) int {
	ok, err := rundir.Claim(rundir.Say, "")
	if err != nil {
		desktop.Notify(err.Error(), "", false)
		return 1
	}
	if !ok {
		desktop.Notify("Already speaking: habla say --stop", "", false)
		return 1
	}
	defer rundir.Release(rundir.Say)
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	wav := filepath.Join(rundir.Dir(), "say.wav")
	defer os.Remove(wav)
	audio, err := client.Speak(ctx, tts, voice, instructions, text)
	if err == nil {
		err = os.WriteFile(wav, audio, 0o600)
	}
	if err == nil {
		err = run(ctx, wav)
	}
	switch {
	case ctx.Err() != nil: // habla say --stop, or Ctrl+C in a terminal
		return 0
	case err != nil:
		slog.Error("say failed", "err", err)
		desktop.Notify(err.Error(), "", false)
		return 1
	}
	return 0
}

// run plays wav with pw-play (PipeWire), else paplay (PulseAudio); cancelling ctx terminates the player and waits for it.
func run(ctx context.Context, wav string) error {
	player, err := desktop.Player()
	if err != nil {
		return err
	}
	slog.Info("player", "tool", filepath.Base(player))
	cmd := exec.CommandContext(ctx, player, wav)
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 5 * time.Second // then SIGKILL
	err = cmd.Start()
	if err == nil {
		if started != nil {
			started(cmd.Process.Pid)
		}
		err = cmd.Wait()
	}
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return fmt.Errorf("Playback failed: %s exited with status %d", filepath.Base(player), exit.ExitCode())
		}
		return err
	}
	return nil
}
