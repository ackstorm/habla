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
	"github.com/ackstorm/habla/internal/logs"
	"github.com/ackstorm/habla/internal/rundir"
	"github.com/ackstorm/habla/internal/testenv"
	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"
	"golang.org/x/sys/unix"
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
	cmd := exec.Command(filepath.Join(e.Bin, "habla"), "tray", "--foreground")
	if privateBus {
		cmd = exec.Command(dbusSession, "--dbus-daemon="+dbusDaemon, "--", filepath.Join(e.Bin, "habla"), "tray", "--foreground")
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

// recordedEnough waits for 300 ms of audio (listen.minRecording): a shorter recording is a tap and sends nothing.
func recordedEnough(t *testing.T) {
	t.Helper()
	testenv.WaitFor(t, "300 ms of audio", func() bool {
		info, err := os.Stat(filepath.Join(rundir.Dir(), "current.wav"))
		return err == nil && info.Size() >= 44+9600
	})
}
func idle() bool { return rundir.RunningPID(rundir.Listen) == 0 }

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

func TestTrayStartsInTheBackgroundGUI(t *testing.T) {
	e, _ := guiEnv(t, "")
	fakeBus(t, e, "none")
	habla := filepath.Join(e.Bin, "habla")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, habla, "tray").CombinedOutput()
	var pid int
	if _, scanErr := fmt.Sscanf(string(out), "Habla tray started (pid %d)", &pid); err != nil || scanErr != nil {
		t.Fatalf("habla tray: %v, %q", err, out)
	}
	t.Cleanup(func() { // the background tray is in a session of its own, outside any test process group
		syscall.Kill(pid, syscall.SIGTERM)
		for i := 0; syscall.Kill(pid, 0) == nil; i++ {
			if i == 50 {
				syscall.Kill(pid, syscall.SIGKILL)
				t.Error("the background tray outlived SIGTERM")
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
	})
	if sid, _ := unix.Getsid(pid); sid != pid {
		t.Errorf("the background tray's session is %d, want its own (%d)", sid, pid)
	}
	out, err = exec.CommandContext(ctx, habla, "tray").CombinedOutput()
	if code := exitCode(err); code != 1 || !strings.Contains(string(out), "Habla tray is already running") {
		t.Errorf("second habla tray: exit %d, %q", code, out)
	}
	xdo(t, "keydown", "super+space") // the background tray grabbed the hotkey
	testenv.WaitFor(t, "recording", recording)
	recordedEnough(t)
	xdo(t, "keyup", "super+space")
	testenv.WaitFor(t, "Hola. copied", func() bool { return slices.Equal(e.Lines("copied"), []string{"Hola."}) })
}

func TestTrayEnterWhileBusySendsGUI(t *testing.T) {
	e, _ := guiEnv(t, "")
	r := testenv.DefaultReplies
	r.HoldSTT = make(chan struct{})
	g := testenv.NewGateway(t, r)
	e.WriteConfig(t, g, "[tray]\nhotkey = \"<Super>Return\"\n")
	release := sync.OnceFunc(func() { close(r.HoldSTT) })
	defer release()
	log, _ := startTray(t, e, true)
	waitLog(t, log, "No tray to show Habla's icon")

	xdo(t, "keydown", "super+Return")
	testenv.WaitFor(t, "recording", recording)
	recordedEnough(t)
	xdo(t, "keyup", "super+Return")
	testenv.WaitFor(t, "transcribing", func() bool { return rundir.ReadState() == "busy" })
	session := rundir.RunningPID(rundir.Listen)
	mark := filepath.Join(rundir.Dir(), "send")
	for _, key := range []string{"Return", "KP_Enter"} {
		testenv.WaitFor(t, key+" while busy to leave the send mark", func() bool {
			xdo(t, "key", key) // the grab follows the busy state through the tray's file watch
			_, err := os.Stat(mark)
			return err == nil
		})
		if err := os.Remove(mark); err != nil {
			t.Fatal(err)
		}
	}
	if got := keyboardGrab(t); got != xproto.GrabStatusAlreadyGrabbed {
		t.Errorf("keyboard grab while sending = %d, want the focus lock (AlreadyGrabbed)", got)
	}
	xdo(t, "key", "Return") // the mark again: the dictation is pasted, then the lock must be gone
	testenv.WaitFor(t, "the send mark", func() bool { _, err := os.Stat(mark); return err == nil })
	release()
	testenv.WaitFor(t, "Hola. copied", func() bool { return slices.Equal(e.Lines("copied"), []string{"Hola."}) })
	testenv.WaitFor(t, "idle", idle)
	// listen.pid goes before copying, so the lock may still be held here: it must go once the paste starts.
	testenv.WaitFor(t, "the focus lock to end", func() bool { return keyboardGrab(t) == xproto.GrabStatusSuccess })
	testenv.WaitFor(t, "the session to finish delivering", func() bool { return !rundir.Alive(session) })
	if data, _ := os.ReadFile(logs.Path()); strings.Contains(string(data), "another app holds the keyboard") {
		t.Errorf("the paste waited for the focus lock:\n%s", data)
	}
	if data, _ := os.ReadFile(log); t.Failed() {
		t.Logf("tray output:\n%s", data)
	}
}

func TestTrayEscCancelsDuringTheFocusLockGUI(t *testing.T) {
	e, _ := guiEnv(t, "")
	r := testenv.DefaultReplies
	r.HoldSTT = make(chan struct{})
	g := testenv.NewGateway(t, r)
	e.WriteConfig(t, g, "")
	defer close(r.HoldSTT)
	log, _ := startTray(t, e, true)
	waitLog(t, log, "No tray to show Habla's icon")

	xdo(t, "keydown", "super+space")
	testenv.WaitFor(t, "recording", recording)
	recordedEnough(t)
	xdo(t, "keyup", "super+space")
	testenv.WaitFor(t, "transcribing", func() bool { return rundir.ReadState() == "busy" })
	testenv.WaitFor(t, "the focus lock after Enter", func() bool {
		xdo(t, "key", "Return")
		return keyboardGrab(t) == xproto.GrabStatusAlreadyGrabbed
	})
	xdo(t, "key", "Escape") // a stuck transcription: Esc must still cancel and unlock
	testenv.WaitFor(t, "idle after Esc", idle)
	testenv.WaitFor(t, "the lock gone after Esc", func() bool { return keyboardGrab(t) == xproto.GrabStatusSuccess })
	if copied := e.Lines("copied"); len(copied) != 0 {
		t.Errorf("copied = %q after Esc, want nothing", copied)
	}
	if data, _ := os.ReadFile(log); t.Failed() {
		t.Logf("tray output:\n%s", data)
	}
}

// keyboardGrab tries to grab the keyboard from a connection of its own (released on close) and returns the status.
func keyboardGrab(t *testing.T) byte {
	t.Helper()
	conn, err := xgb.NewConn()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	root := xproto.Setup(conn).DefaultScreen(conn).Root
	r, err := xproto.GrabKeyboard(conn, false, root, xproto.TimeCurrentTime, xproto.GrabModeAsync, xproto.GrabModeAsync).Reply()
	if err != nil {
		t.Fatal(err)
	}
	return r.Status
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
