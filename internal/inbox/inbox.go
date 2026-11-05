// Package inbox is `habla inbox`: speak what agents send with voice_say on an Artifact Server.
//
// habla registers as an agent of kind habla and long-polls its voice route. The poll is its heartbeat: while it
// runs, voice_say queues sentences for this user; otherwise voice_say queues nothing. The server hands each message
// over once and drops it after 60 s.
package inbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	"strings"
	"syscall"
	"time"

	"github.com/ackstorm/habla/internal/config"
	"github.com/ackstorm/habla/internal/desktop"
	"github.com/ackstorm/habla/internal/gateway"
	"github.com/ackstorm/habla/internal/logs"
	"github.com/ackstorm/habla/internal/rundir"
)

// Retry is the pause after a failed poll. ponytail: a var only so tests can shorten it.
var Retry = 5 * time.Second

// Wait is the long poll's wait in seconds (the server caps it at 25). ponytail: a var only so tests can shorten it.
var Wait = 25

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
		fmt.Fprintf(os.Stderr, "Listening for voice_say on %s (Ctrl+C to stop)\n", cfg.Inbox.Server)
	}
	if err := Run(ctx, cfg.Inbox, key); err != nil {
		desktop.Notify("Inbox stopped: "+err.Error(), "", false)
		return 1
	}
	return 0
}

// Run listens until ctx ends. It returns an error only when the server refuses the key.
func Run(ctx context.Context, in *config.Inbox, key string) error {
	c := &client{in: in, key: key, http: &http.Client{Timeout: time.Duration(Wait+10) * time.Second}}
	defer c.disconnect()
	for ctx.Err() == nil {
		err := c.poll(ctx)
		switch {
		case err == nil:
			continue
		case errors.Is(err, errRefused), errors.Is(err, errNoCapability):
			return err
		case errors.Is(err, errUnknownAgent):
			c.agent = "" // reaped or reassigned: register again
			continue
		case ctx.Err() != nil:
			return nil
		}
		slog.Warn("inbox poll failed", "err", err)
		select {
		case <-ctx.Done():
		case <-time.After(Retry):
		}
	}
	return nil
}

var (
	errRefused      = errors.New("the Artifact Server refused the inbox api_key")
	errNoCapability = errors.New("the inbox api_key lacks agent:connect (Connect agents)")
	errUnknownAgent = errors.New("the Artifact Server no longer knows this listener")
)

type client struct {
	in    *config.Inbox
	key   string
	http  *http.Client
	agent string // registered agent id; "" until registered
}

// poll registers when needed, then waits for messages and speaks them in order.
func (c *client) poll(ctx context.Context) error {
	if c.agent == "" {
		if err := c.register(ctx); err != nil {
			return err
		}
	}
	var got struct {
		Messages []struct {
			Text string `json:"text"`
		} `json:"messages"`
	}
	u := fmt.Sprintf("%s/api/v1/agents/%s/voice?wait=%d", c.in.Server, url.PathEscape(c.agent), Wait)
	if err := c.do(ctx, http.MethodPost, u, nil, &got); err != nil {
		return err
	}
	for _, m := range got.Messages {
		waitForSilence(ctx)
		slog.Info("inbox message", "chars", len(m.Text))
		if err := Speak(ctx, strings.TrimSpace(m.Text)); err != nil && ctx.Err() == nil {
			slog.Warn("inbox speak failed", "err", err)
		}
	}
	return nil
}

func (c *client) register(ctx context.Context) error {
	host, _ := os.Hostname()
	home, _ := os.UserHomeDir()
	sum := sha256.Sum256([]byte(host + "\x00habla-inbox"))
	body, _ := json.Marshal(map[string]any{
		"connectionKey":    hex.EncodeToString(sum[:]),
		"displayName":      "habla on " + host,
		"kind":             "habla",
		"workingDirectory": home,
	})
	var got struct {
		Agent struct {
			ID string `json:"id"`
		} `json:"agent"`
	}
	if err := c.do(ctx, http.MethodPost, c.in.Server+"/api/v1/agents", body, &got); err != nil {
		if errors.Is(err, errUnknownAgent) { // 403 here: the key may not register agents
			return errNoCapability
		}
		return err
	}
	if got.Agent.ID == "" {
		return errors.New("the Artifact Server registered no agent")
	}
	c.agent = got.Agent.ID
	return nil
}

// disconnect tells the server this listener is gone, so voice_say stops queueing at once (best effort).
func (c *client) disconnect() {
	if c.agent == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c.do(ctx, http.MethodPost, c.in.Server+"/api/v1/agents/"+url.PathEscape(c.agent)+"/disconnect", nil, nil)
}

// waitForSilence waits up to 30 s while another habla say plays, so messages never cut each other.
func waitForSilence(ctx context.Context) {
	for i := 0; i < 60 && rundir.RunningPID(rundir.Say) != 0 && ctx.Err() == nil; i++ {
		select {
		case <-ctx.Done():
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (c *client) do(ctx context.Context, method, u string, body []byte, into any) error {
	req, err := http.NewRequestWithContext(ctx, method, u, strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return errRefused
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusForbidden:
		return errUnknownAgent
	case resp.StatusCode == http.StatusNoContent:
		return nil
	case resp.StatusCode/100 != 2:
		return fmt.Errorf("%s %s: HTTP %d", method, req.URL.Path, resp.StatusCode)
	case into != nil:
		return json.NewDecoder(resp.Body).Decode(into)
	}
	return nil
}
