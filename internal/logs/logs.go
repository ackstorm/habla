// Package logs sends log/slog records to $XDG_STATE_HOME/habla/habla.log, and to stderr when it is a terminal.
package logs

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ackstorm/habla/internal/config"
	"github.com/ackstorm/habla/internal/desktop"
)

var (
	level  slog.LevelVar
	forced bool // --log was given: log_level does not apply
)

// Path is $XDG_STATE_HOME/habla/habla.log.
func Path() string { return filepath.Join(config.StateDir(), "habla.log") }

// Setup makes slog write `<local time> command[pid] LEVEL [delta_ms=N] message key=value…` lines at the --log level flag (""
// = warn until SetLevel applies log_level). Only an unknown level is an error: a log that can't be written is skipped.
func Setup(command, flag string) error {
	if flag == "" {
		flag = os.Getenv("HABLA_LOG") // set by the habla process that started this one
	}
	if forced = flag != ""; forced && !slices.Contains(config.LogLevels, flag) {
		return fmt.Errorf("--log: expected %s", strings.Join(config.LogLevels, " | "))
	} else if forced {
		os.Setenv("HABLA_LOG", flag) // for the habla processes this one starts (listen --session, the tray's runs)
	} else {
		flag = "warn"
	}
	level.UnmarshalText([]byte(flag))
	path := Path()
	os.MkdirAll(filepath.Dir(path), 0o700)
	os.Chmod(filepath.Dir(path), 0o700)
	if info, err := os.Stat(path); err == nil && info.Size() > 1<<20 {
		os.Rename(path, path+".1") // ponytail: two processes rotating at once may lose one's first lines
	}
	var tty io.Writer
	if desktop.Terminal(os.Stderr) {
		tty = os.Stderr
	}
	slog.SetDefault(slog.New(&handler{prefix: fmt.Sprintf("%s[%d]", command, os.Getpid()), path: path, tty: tty}))
	return nil
}

// SetLevel applies log_level, unless --log was given.
func SetLevel(name string) {
	if !forced {
		level.UnmarshalText([]byte(name))
	}
}

// handler writes the line format above; slog.TextHandler can't drop its time=, level= and msg= keys.
type handler struct {
	mu     sync.Mutex
	last   time.Time
	prefix string    // listen[4321]
	path   string    // habla.log
	tty    io.Writer // stderr on a terminal: info and debug only, warnings and errors print their own message
}

func (h *handler) Enabled(_ context.Context, l slog.Level) bool { return l >= level.Level() }

func (h *handler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	// Timestamp in write order so concurrent callers cannot produce negative gaps.
	now := time.Now()
	var delta int64
	if !h.last.IsZero() {
		delta = now.Sub(h.last).Milliseconds()
	}
	h.last = now
	b := fmt.Appendf(nil, "%s %s %s [delta_ms=%d] %s", now.Format("2006-01-02T15:04:05.000"), h.prefix, r.Level, delta, r.Message)
	r.Attrs(func(a slog.Attr) bool {
		v := a.Value.String()
		if v == "" || strings.ContainsAny(v, " \"=\n") {
			v = strconv.Quote(v)
		}
		b = fmt.Appendf(b, " %s=%s", a.Key, v)
		return true
	})
	b = append(b, '\n')
	if h.tty != nil && r.Level < slog.LevelWarn {
		h.tty.Write(b)
	}
	// Opened per line, so a long-lived tray follows the rotation. One write: with O_APPEND, lines never interleave.
	f, err := os.OpenFile(h.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE|syscall.O_CLOEXEC, 0o600)
	if err != nil {
		return nil // logging never breaks dictation
	}
	defer f.Close()
	_, err = f.Write(b)
	return err
}

func (h *handler) WithAttrs([]slog.Attr) slog.Handler { return h } // ponytail: nothing uses slog.With or groups
func (h *handler) WithGroup(string) slog.Handler      { return h }
