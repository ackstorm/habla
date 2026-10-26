package tray

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/ackstorm/habla/internal/desktop"
	"github.com/ackstorm/habla/internal/testenv"
)

type note struct {
	summary, body string
	desktop       bool
}

// portalEnv is testenv.Setup plus a private data directory and recorded notifications. A mode other than "no bus"
// starts testenv's fake portal in that mode (fakeBus).
func portalEnv(t *testing.T, mode string) (e *testenv.Env, notes func() []note) {
	t.Helper()
	e = testenv.Setup(t)
	t.Setenv("XDG_DATA_HOME", filepath.Join(e.Dir, "data"))
	var mu sync.Mutex
	var got []note
	saved := desktop.Notify
	desktop.Notify = func(summary, body string, d bool) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, note{summary, body, d})
	}
	t.Cleanup(func() { desktop.Notify = saved })
	notes = func() []note {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(got)
	}
	if mode == "no bus" {
		return e, notes
	}
	if dbusSession == "" || dbusDaemon == "" {
		t.Skip("dbus-run-session is not installed")
	}
	fakeBus(t, e, mode)
	return e, notes
}

// fakeBus starts testenv's fake portal (and notification daemon) in mode on a private bus (dbus-run-session) and
// points the session bus at it.
func fakeBus(t *testing.T, e *testenv.Env, mode string) {
	t.Helper()
	t.Setenv("HABLA_FAKE_PORTAL", mode)
	os.MkdirAll(os.Getenv("XDG_RUNTIME_DIR"), 0o700) // dbus-daemon warns without it
	cmd := exec.Command(dbusSession, "--dbus-daemon="+dbusDaemon, "--", filepath.Join(e.Bin, "fake-portal"))
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		cmd.Wait()
	})
	address := make(chan string, 1)
	go func() {
		s := bufio.NewScanner(out)
		s.Scan()
		address <- s.Text()
	}()
	select {
	case a := <-address:
		if a == "" {
			t.Fatal("the fake portal did not start")
		}
		t.Setenv("DBUS_SESSION_BUS_ADDRESS", a)
	case <-time.After(10 * time.Second):
		t.Fatal("the fake portal did not start within 10 s")
	}
}

func TestPortalHoldToTalk(t *testing.T) {
	e, notes := portalEnv(t, "bind")
	acts := make(chan string, 10)
	go runPortal("LOGO+space", func(flag string) { acts <- flag })
	for _, want := range []string{"--start", "--stop", "--start", "--stop", "--start", "--stop", "--send", "--cancel"} {
		select {
		case got := <-acts:
			if got != want {
				t.Fatalf("act %q, want %q", got, want)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("no %s within 10 s; portal calls %q, notifications %v", want, e.Lines("portal"), notes())
		}
	}
	select {
	case got := <-acts:
		t.Errorf("act %q after the last release (the other shortcut or session)", got)
	case <-time.After(200 * time.Millisecond):
	}
	want := []string{"Register " + AppID, "Get org.freedesktop.portal.GlobalShortcuts version", "CreateSession",
		"BindShortcuts dictate LOGO+space Hold to dictate",
		"BindShortcuts send LOGO+Return Send the dictation (paste, then Enter)",
		"BindShortcuts cancel LOGO+Escape Discard the dictation"}
	if calls := e.Lines("portal"); !slices.Equal(calls, want) {
		t.Errorf("portal calls %q, want %q", calls, want)
	}
	if n := notes(); len(n) > 0 {
		t.Errorf("notifications %v", n)
	}
	if _, err := os.Stat(filepath.Join(e.Dir, "data", "applications", AppID+".desktop")); err != nil {
		t.Error(err) // the portal finds the app ID through it
	}
}

func TestPortalFailuresNotifyOnce(t *testing.T) {
	const off = "Bind habla listen --toggle to a shortcut (see the README)."
	missing := "Hold-to-talk needs the GlobalShortcuts portal: bind habla listen --toggle instead"
	for mode, want := range map[string]note{
		"decline": {"Hold-to-talk is off: BindShortcuts cancelled", off, true},
		"fail":    {"Hold-to-talk is off: BindShortcuts failed", off, true},
		"none":    {missing, "No such interface “org.freedesktop.portal.GlobalShortcuts”", false},
		"stall":   {"Hold-to-talk is off: context deadline exceeded", off, true},
		"no bus":  {missing, "", false},
	} {
		t.Run(mode, func(t *testing.T) {
			_, notes := portalEnv(t, mode)
			saved := callTimeout
			callTimeout = 500 * time.Millisecond
			t.Cleanup(func() { callTimeout = saved })
			done := make(chan struct{})
			go func() {
				runPortal("LOGO+space", func(flag string) { t.Errorf("act %s", flag) })
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatalf("runPortal still running after 10 s; notifications %v", notes())
			}
			n := notes()
			if mode == "no bus" && len(n) == 1 {
				want.body = n[0].body // the dial error
			}
			if len(n) != 1 || n[0] != want {
				t.Errorf("notifications %v, want [%v]", n, want)
			}
		})
	}
}
