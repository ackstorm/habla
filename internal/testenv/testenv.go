// Package testenv is the shared test setup: private XDG directories, fake tools on PATH, and bounded waits.
// Only _test.go files import it.
package testenv

import (
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakes are the tools the test binary pretends to be when it runs through a link of that name (see Fake).
var fakes = map[string]func(args []string) int{}

// Fake runs the fake tool named by os.Args[0] and exits. Call it first in TestMain.
func Fake() {
	if os.Getenv("HABLA_FAKE_SLEEP") != "" { // HablaProcess
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	if run, ok := fakes[filepath.Base(os.Args[0])]; ok {
		os.Exit(run(os.Args[1:]))
	}
}

// Env is one test's private directory (Dir) and the PATH directory of fakes (Bin).
type Env struct {
	Dir string
	Bin string
}

// Setup gives the test private XDG directories, $HABLA_API_KEY, a Wayland session with no real display or bus, and
// a PATH that holds only links to the test binary: every fake, plus "habla" (TestMain decides what that runs).
func Setup(t *testing.T) *Env {
	t.Helper()
	dir := t.TempDir()
	e := &Env{Dir: dir, Bin: filepath.Join(dir, "bin")}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(e.Bin, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range append(slices.Sorted(maps.Keys(fakes)), "habla") {
		if err := os.Symlink(exe, filepath.Join(e.Bin, name)); err != nil {
			t.Fatal(err)
		}
	}
	for k, v := range map[string]string{
		"PATH":                     e.Bin,
		"XDG_RUNTIME_DIR":          filepath.Join(dir, "runtime"),
		"XDG_CONFIG_HOME":          filepath.Join(dir, "config"),
		"XDG_STATE_HOME":           filepath.Join(dir, "state"),
		"HABLA_API_KEY":            "sk-test",
		"HABLA_LOG":                "",
		"HABLA_FAKE_DIR":           dir,
		"HABLA_FAKE_FAIL":          "",
		"HABLA_FAKE_COPY_GATE":     "",
		"HABLA_FAKE_PASTE_GATE":    "",
		"HABLA_FAKE_YDOTOOL":       "",
		"WAYLAND_DISPLAY":          "wayland-test",
		"XDG_SESSION_TYPE":         "wayland",
		"DISPLAY":                  "",
		"DBUS_SESSION_BUS_ADDRESS": "unix:path=" + filepath.Join(dir, "no-bus"),
	} {
		t.Setenv(k, v)
	}
	return e
}

// Remove takes a fake tool off PATH.
func (e *Env) Remove(name string) { os.Remove(filepath.Join(e.Bin, name)) }

// Lines returns the lines the fakes appended to the file name in the test directory (nil when none).
func (e *Env) Lines(name string) []string {
	data, err := os.ReadFile(filepath.Join(e.Dir, name))
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

// appendLine is how fakes, which run as separate processes, report what they were asked to do.
func appendLine(name, line string) {
	f, err := os.OpenFile(filepath.Join(os.Getenv("HABLA_FAKE_DIR"), name), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintln(f, line)
}

// failing reports whether $HABLA_FAKE_FAIL (a comma-separated list) names this fake.
func failing(name string) bool {
	return slices.Contains(strings.Split(os.Getenv("HABLA_FAKE_FAIL"), ","), name)
}

// WaitFor checks cond every 10 ms for up to 30 s and fails the test if it never holds.
// ponytail: 30 s, not tighter: GTK start-up on a cold CI runner took over 10 s (the tray check fires 5 s after).
func WaitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// HablaProcess starts a process that rundir.Alive accepts (the test binary sleeping through a link named habla; its
// TestMain must call Fake); it is killed when the test ends. Not /bin/sleep: uutils coreutils (Ubuntu 26.04) is one
// binary that picks the tool by its link name, so "habla" exits at once.
func HablaProcess(t *testing.T) *exec.Cmd {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "habla")
	if err := os.Symlink(exe, link); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(link)
	cmd.Env = append(os.Environ(), "HABLA_FAKE_SLEEP=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
	})
	// Start returns as soon as the child execs, but the kernel fills /proc/<pid>/cmdline a moment later.
	WaitFor(t, "the process's command line", func() bool {
		data, _ := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", cmd.Process.Pid))
		return len(data) > 0
	})
	return cmd
}

// Stderr runs f and returns what it wrote to os.Stderr.
func Stderr(t *testing.T, f func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	out := make(chan []byte)
	go func() { data, _ := io.ReadAll(r); out <- data }()
	saved := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = saved }()
	f()
	w.Close()
	return string(<-out)
}
