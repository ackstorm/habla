// Package inbox is `habla inbox`: speak the messages other agents leave on an Artifact Server, then delete them.
//
// A message is an artifact tagged [inbox] tag; its name is the text. Messages older than max_age are deleted
// unspoken: nobody was listening when they arrived.
package inbox

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/ackstorm/habla/internal/config"
	"github.com/ackstorm/habla/internal/desktop"
	"github.com/ackstorm/habla/internal/gateway"
	"github.com/ackstorm/habla/internal/logs"
	"github.com/ackstorm/habla/internal/rundir"
)

// Interval is the time between two checks. ponytail: a var only so tests can shorten it.
var Interval = 5 * time.Second

// Speak says one message; it waits until it has been played. ponytail: a var only so tests can record the messages.
var Speak = func(ctx context.Context, text string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	return exec.CommandContext(ctx, self, "say", text).Run()
}

const notConfigured = "Inbox is not configured: add an [inbox] section to config.toml"

func Main(args []string) int {
	fs := flag.NewFlagSet("habla inbox", flag.ContinueOnError)
	logLevel := fs.String("log", "", "log at `LEVEL` (error, warn, info, debug) for this run")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if err := logs.Setup("inbox", *logLevel); err != nil {
		fmt.Fprintln(os.Stderr, "habla inbox:", err)
		return 2
	}
	cfg, err := config.Load()
	if err != nil {
		desktop.ConfigError(err, !desktop.Terminal(os.Stderr))
		return 1
	}
	logs.SetLevel(cfg.LogLevel)
	if cfg.Inbox == nil {
		desktop.Notify(notConfigured, "", false)
		return 1
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	key, err := gateway.Credential(ctx, cfg.Inbox.APIKey)
	if err != nil {
		desktop.Notify(err.Error(), "", false)
		return 1
	}
	if desktop.Terminal(os.Stderr) {
		fmt.Fprintf(os.Stderr, "Listening for #%s on %s (Ctrl+C to stop)\n", cfg.Inbox.Tag, cfg.Inbox.Server)
	}
	if err := Run(ctx, cfg.Inbox, key); err != nil {
		desktop.Notify("Inbox stopped: "+err.Error(), "", false)
		return 1
	}
	return 0
}

// Run checks the inbox every Interval until ctx ends. It returns an error only when the server refuses the key.
func Run(ctx context.Context, in *config.Inbox, key string) error {
	c := &client{in: in, key: key, http: &http.Client{Timeout: 15 * time.Second}}
	spoken := map[string]bool{} // messages already handled, in case the server refuses to delete one
	for {
		if err := c.check(ctx, spoken); err != nil {
			if errors.Is(err, errRefused) {
				return err
			}
			slog.Warn("inbox check failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(Interval):
		}
	}
}

type message struct {
	ID               string    `json:"id"`
	Name             string    `json:"name"`
	CreatedAt        time.Time `json:"createdAt"`
	CurrentVersionID string    `json:"currentVersionId"`
}

var errRefused = errors.New("the Artifact Server refused the inbox api_key")

type client struct {
	in   *config.Inbox
	key  string
	http *http.Client
}

// check speaks each fresh message, oldest first, and deletes every message it sees.
func (c *client) check(ctx context.Context, spoken map[string]bool) error {
	msgs, err := c.list(ctx)
	if err != nil {
		return err
	}
	slices.SortFunc(msgs, func(a, b message) int { return a.CreatedAt.Compare(b.CreatedAt) })
	for _, m := range msgs {
		if spoken[m.ID] {
			continue
		}
		if rundir.RunningPID(rundir.Say) != 0 { // someone else is speaking: try again at the next check
			return nil
		}
		spoken[m.ID] = true
		if err := c.delete(ctx, m); err != nil {
			slog.Warn("inbox delete failed", "id", m.ID, "err", err)
		}
		age := time.Since(m.CreatedAt)
		if age > time.Duration(c.in.MaxAge)*time.Second {
			slog.Info("inbox message expired", "id", m.ID, "age_s", int(age.Seconds()))
			continue
		}
		slog.Info("inbox message", "id", m.ID, "chars", len(m.Name))
		if err := Speak(ctx, strings.TrimSpace(m.Name)); err != nil && ctx.Err() == nil {
			slog.Warn("inbox speak failed", "id", m.ID, "err", err)
		}
	}
	return nil
}

func (c *client) list(ctx context.Context) ([]message, error) {
	u := c.in.Server + "/api/v1/artifacts?limit=100&tag=" + url.QueryEscape(c.in.Tag)
	var page struct {
		Artifacts []struct {
			Artifact message `json:"artifact"`
		} `json:"artifacts"`
	}
	if err := c.do(ctx, http.MethodGet, u, "", nil, &page); err != nil {
		return nil, err
	}
	msgs := make([]message, len(page.Artifacts))
	for i, a := range page.Artifacts {
		msgs[i] = a.Artifact
	}
	return msgs, nil
}

func (c *client) delete(ctx context.Context, m message) error {
	body, _ := json.Marshal(map[string]string{"expectedCurrentVersionId": m.CurrentVersionID})
	return c.do(ctx, http.MethodDelete, c.in.Server+"/api/v1/artifacts/"+url.PathEscape(m.ID), "habla-inbox-delete-"+m.ID, body, nil)
}

func (c *client) do(ctx context.Context, method, u, idempotency string, body []byte, into any) error {
	req, err := http.NewRequestWithContext(ctx, method, u, strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if idempotency != "" {
		req.Header.Set("Idempotency-Key", idempotency)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return errRefused
	case resp.StatusCode/100 != 2:
		return fmt.Errorf("%s %s: HTTP %d", method, req.URL.Path, resp.StatusCode)
	case into != nil:
		return json.NewDecoder(resp.Body).Decode(into)
	}
	return nil
}
