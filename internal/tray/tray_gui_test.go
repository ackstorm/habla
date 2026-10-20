package tray

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/ackstorm/habla/internal/desktop"
	"github.com/ackstorm/habla/internal/rundir"
	"github.com/ackstorm/habla/internal/testenv"
	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"
)

// Looked up before any test replaces PATH with fakes.
var (
	goTool, _      = exec.LookPath("go")
	dbusSession, _ = exec.LookPath("dbus-run-session")
	dbusDaemon, _  = exec.LookPath("dbus-daemon")
	xdotool, _     = exec.LookPath("xdotool")
	built          string // the real habla binary, built by run under HABLA_GUI=1
)

// run builds habla (CGO_ENABLED=0, as released) for the GUI tests, then runs the tests.
func run(m *testing.M) int {
	if os.Getenv("HABLA_GUI") != "1" {
		return m.Run()
	}
	dir, err := os.MkdirTemp("", "habla-gui-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer os.RemoveAll(dir)
	built = filepath.Join(dir, "habla")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, goTool, "build", "-o", built, "github.com/ackstorm/habla")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOTOOLCHAIN=local")
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "building habla:", err)
		return 1
	}
	return m.Run()
}

// guiEnv is a private Xvfb, the testenv fakes, a fake gateway, and the real habla on PATH.
func guiEnv(t *testing.T, extraConfig string) (*testenv.Env, *testenv.Gateway) {
	t.Helper()
	if os.Getenv("HABLA_GUI") != "1" {
		t.Skip("set HABLA_GUI=1 to run habla tray under Xvfb")
	}
	for name, path := range map[string]string{"dbus-run-session": dbusSession, "dbus-daemon": dbusDaemon, "xdotool": xdotool} {
		if path == "" {
			t.Fatalf("%s is not installed", name)
		}
	}
	display := testenv.Xvfb(t)
	e := testenv.Setup(t)
	e.Remove("habla")
	if err := os.Symlink(built, filepath.Join(e.Bin, "habla")); err != nil {
		t.Fatal(err)
	}
	g := testenv.NewGateway(t, testenv.DefaultReplies)
	e.WriteConfig(t, g, extraConfig)
	for k, v := range map[string]string{
		"DISPLAY": display, "WAYLAND_DISPLAY": "", "XDG_SESSION_TYPE": "x11", "GDK_BACKEND": "x11",
		"HOME": filepath.Join(e.Dir, "home"), "XDG_DATA_HOME": filepath.Join(e.Dir, "data"), "NO_AT_BRIDGE": "1",
	} {
		t.Setenv(k, v)
	}
	t.Cleanup(func() { // a recording left by a failed test
		exec.Command(filepath.Join(e.Bin, "habla"), "listen", "--cancel").Run()
	})
	return e, g
}

// startTray runs `habla tray` on a private session bus, or with privateBus false on the session bus already set
// (fakeBus). Its output goes to the returned log; stop (also run at cleanup) kills it. A GLib CRITICAL in the
// output fails the test.
func startTray(t *testing.T, e *testenv.Env, privateBus bool) (log string, stop func()) {
	t.Helper()
	log = filepath.Join(e.Dir, "tray.log")
	f, err := os.Create(log)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	cmd := exec.Command(filepath.Join(e.Bin, "habla"), "tray")
	if privateBus {
		cmd = exec.Command(dbusSession, "--dbus-daemon="+dbusDaemon, "--", filepath.Join(e.Bin, "habla"), "tray")
	}
	cmd.Stdout, cmd.Stderr = f, f
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { cmd.Wait(); close(exited) }()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
			select {
			case <-exited:
			case <-time.After(5 * time.Second):
				syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				<-exited
			}
			// dbus-run-session exits on SIGTERM without waiting for the tray and its bus daemon: wait for the whole
			// group, or the tray still writes into the test's directories while their cleanup removes them.
			for i := 0; syscall.Kill(-cmd.Process.Pid, 0) == nil; i++ {
				if i == 50 {
					syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				}
				if i == 100 {
					t.Error("the tray's processes outlived SIGKILL")
					break
				}
				time.Sleep(100 * time.Millisecond)
			}
		})
	}
	t.Cleanup(func() {
		stop()
		if data, _ := os.ReadFile(log); strings.Contains(string(data), "-CRITICAL **") {
			t.Errorf("GTK criticals in the tray's output:\n%s", data) // the menu rebuilds on every recording
		}
	})
	return log, stop
}

func waitLog(t *testing.T, log, text string) {
	t.Helper()
	testenv.WaitFor(t, fmt.Sprintf("%q in the tray's output", text), func() bool {
		data, _ := os.ReadFile(log)
		return strings.Contains(string(data), text)
	})
}

func xdo(t *testing.T, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, xdotool, args...).CombinedOutput(); err != nil {
		t.Fatalf("xdotool %v: %v\n%s", args, err, out)
	}
}

func recording() bool { return rundir.ReadState() == "recording" }
func idle() bool      { return rundir.RunningPID(rundir.Listen) == 0 }

func TestTrayHoldToTalkGUI(t *testing.T) {
	e, g := guiEnv(t, "")
	log, _ := startTray(t, e, true)
	// newTray grabs the hotkey before it sets the 5 s tray-host check; Xvfb has no tray host, so this notice comes.
	waitLog(t, log, "No tray to show Habla's icon")

	xdo(t, "keydown", "super+space")
	testenv.WaitFor(t, "recording", recording)
	time.Sleep(1200 * time.Millisecond) // past the X auto-repeat delay: repeats must not end the recording
	if !recording() {
		t.Fatal("the recording ended while the key was held")
	}
	xdo(t, "keyup", "super+space")
	testenv.WaitFor(t, "Hola. copied", func() bool { return slices.Equal(e.Lines("copied"), []string{"Hola."}) })
	testenv.WaitFor(t, "idle", idle)

	xdo(t, "keydown", "super+space")
	testenv.WaitFor(t, "the second recording", recording)
	xdo(t, "key", "Escape")
	xdo(t, "keyup", "super+space")
	testenv.WaitFor(t, "idle after Esc", idle)
	if copied := e.Lines("copied"); len(copied) != 1 {
		t.Errorf("copied = %q, want only the first dictation", copied)
	}
	if n := len(g.Audio()); n != 1 {
		t.Errorf("%d transcriptions, want 1", n)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, filepath.Join(e.Bin, "habla"), "tray").CombinedOutput()
	if code := exitCode(err); code != 1 || !strings.Contains(string(out), "Habla tray is already running") {
		t.Errorf("second tray: exit %d, %q", code, out)
	}
	if data, _ := os.ReadFile(log); t.Failed() {
		t.Logf("tray output:\n%s", data)
	}
}

func TestTrayHotkeyProblemsGUI(t *testing.T) {
	for name, tc := range map[string]struct{ hotkey, want string }{
		"unknown key": {"<Super>nokey", "Invalid hotkey <Super>nokey: unknown key 'nokey'"},
		"taken":       {"<Super>space", "<Super>space is taken by another app"},
	} {
		t.Run(name, func(t *testing.T) {
			e, _ := guiEnv(t, fmt.Sprintf("[tray]\nhotkey = %q\n", tc.hotkey))
			if name == "taken" {
				conn, err := xgb.NewConn()
				if err != nil {
					t.Fatal(err)
				}
				defer conn.Close()
				setup := xproto.Setup(conn)
				codes, err := desktop.Keycodes(conn, setup)
				if err != nil {
					t.Fatal(err)
				}
				root := setup.DefaultScreen(conn).Root
				if err := xproto.GrabKeyChecked(conn, true, root, 64, codes[0x20], xproto.GrabModeAsync, xproto.GrabModeAsync).Check(); err != nil {
					t.Fatal(err)
				}
			}
			log, _ := startTray(t, e, true)
			waitLog(t, log, tc.want)
			waitLog(t, log, fmt.Sprintf("Change the line hotkey = %q in ", tc.hotkey))
			waitLog(t, log, "No tray to show Habla's icon") // the tray keeps running
		})
	}
}

func exitCode(err error) int {
	if exit, ok := err.(*exec.ExitError); ok {
		return exit.ExitCode()
	}
	if err != nil {
		return -1
	}
	return 0
}
