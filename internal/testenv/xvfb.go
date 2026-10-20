package testenv

import (
	"bufio"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// xvfb is looked up before any test replaces PATH with fakes.
var xvfb, _ = exec.LookPath("Xvfb")

// Xvfb starts a private X server (1280x800x24, no TCP) and returns its display, such as ":5". The server picks a free
// display number itself (-displayfd) and reports it once it accepts connections; it is killed when the test ends.
func Xvfb(t *testing.T) string {
	t.Helper()
	if xvfb == "" {
		t.Fatal("Xvfb is not installed")
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	cmd := exec.Command(xvfb, "-displayfd", "3", "-screen", "0", "1280x800x24", "-nolisten", "tcp")
	cmd.ExtraFiles = []*os.File{w}
	err = cmd.Start()
	w.Close()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
	})
	line := make(chan string, 1)
	go func() {
		s, _ := bufio.NewReader(r).ReadString('\n')
		line <- strings.TrimSpace(s)
	}()
	select {
	case n := <-line:
		if n == "" {
			t.Fatal("Xvfb exited before reporting its display")
		}
		return ":" + n
	case <-time.After(10 * time.Second):
		t.Fatal("Xvfb did not report its display within 10 s")
	}
	return ""
}
