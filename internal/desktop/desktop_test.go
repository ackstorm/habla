package desktop_test

import (
	"io"
	"os"
	"slices"
	"testing"

	"github.com/ackstorm/habla/internal/desktop"
	"github.com/ackstorm/habla/internal/testenv"
)

func TestMain(m *testing.M) {
	testenv.Fake()
	os.Exit(m.Run())
}

func wantLines(t *testing.T, got []string, want ...string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestCopyUsesTheSessionTool(t *testing.T) {
	e := testenv.Setup(t)
	if err := desktop.Copy("hola"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WAYLAND_DISPLAY", "")
	t.Setenv("XDG_SESSION_TYPE", "x11")
	if err := desktop.Copy("adiós"); err != nil {
		t.Fatal(err)
	}
	wantLines(t, e.Lines("copied"), "hola", "adiós")
	wantLines(t, e.Lines("copy-args"), "wl-copy", "xclip -selection clipboard")
}

func TestCopyUsesWaylandDisplayInATTYSession(t *testing.T) {
	e := testenv.Setup(t)
	t.Setenv("XDG_SESSION_TYPE", "tty")
	desktop.Copy("x")
	wantLines(t, e.Lines("copy-args"), "wl-copy")
}

func TestCopyWithoutTheTool(t *testing.T) {
	e := testenv.Setup(t)
	e.Remove("wl-copy")
	if err := desktop.Copy("x"); err == nil || err.Error() != "wl-copy is not installed (package wl-clipboard)" {
		t.Fatal(err)
	}
}

func TestCopyToolFailure(t *testing.T) {
	testenv.Setup(t)
	t.Setenv("HABLA_FAKE_FAIL", "wl-copy")
	if err := desktop.Copy("x"); err == nil || err.Error() != "wl-copy failed" {
		t.Fatal(err)
	}
}

func TestWaylandPasteToolSelection(t *testing.T) {
	for _, tc := range []struct {
		name, keys, ydotool string
		remove              []string
		pasted, err         string
	}{
		{name: "wtype first", keys: "auto", pasted: "wtype -M ctrl v -m ctrl"},
		{name: "ydotool 1.x", keys: "ctrl+shift+v", remove: []string{"wtype"}, pasted: "ydotool key 29:1 42:1 47:1 47:0 42:0 29:0"},
		{name: "ydotool 0.1", keys: "auto", ydotool: "0.1", remove: []string{"wtype"}, err: "ydotool 0.1.x is not supported: install ydotool 1.x"},
		{name: "old ydotool skipped", keys: "ctrl+v", ydotool: "0.1", pasted: "wtype -M ctrl v -m ctrl"},
		{name: "none", keys: "auto", remove: []string{"wtype", "ydotool"}, err: "no paste tool: install wtype or ydotool 1.x"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := testenv.Setup(t)
			t.Setenv("HABLA_FAKE_YDOTOOL", tc.ydotool)
			for _, name := range tc.remove {
				e.Remove(name)
			}
			err := desktop.Paste(tc.keys, false)
			if tc.err != "" {
				if err == nil || err.Error() != tc.err {
					t.Fatal(err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			wantLines(t, e.Lines("pasted"), tc.pasted)
		})
	}
}

func TestPasteFallsBackFromWtypeToYdotool(t *testing.T) {
	e := testenv.Setup(t)
	t.Setenv("HABLA_FAKE_FAIL", "wtype") // wtype fails on KDE and GNOME
	if err := desktop.Paste("auto", false); err != nil {
		t.Fatal(err)
	}
	wantLines(t, e.Lines("pasted"), "ydotool key 29:1 47:1 47:0 29:0")
}

func TestPasteFailureNamesEveryToolTried(t *testing.T) {
	testenv.Setup(t)
	t.Setenv("HABLA_FAKE_FAIL", "wtype,ydotool")
	if err := desktop.Paste("auto", false); err == nil || err.Error() != "wtype failed; ydotool failed: is ydotoold running?" {
		t.Fatal(err)
	}
}

func TestTerminalDetection(t *testing.T) {
	for names, want := range map[string]bool{
		"mate-terminal\x00Mate-terminal\x00": true,
		"kitty\x00kitty\x00":                 true,
		"tilix\x00Com.gexperts.Tilix\x00":    true,
		"Navigator\x00firefox\x00":           false,
		"code\x00Code\x00":                   false,
		"":                                   false,
	} {
		if got := desktop.IsTerminal(desktop.ClassNames([]byte(names))); got != want {
			t.Errorf("%q: %v", names, got)
		}
	}
	if got := desktop.ClassNames([]byte("xterm\x00XTerm\x00")); !slices.Equal(got, []string{"xterm", "XTerm"}) {
		t.Fatal(got)
	}
}

func TestNotifyAlwaysPrintsToStderr(t *testing.T) {
	testenv.Setup(t) // no session bus: the desktop notification fails quietly
	r, w, _ := os.Pipe()
	saved := os.Stderr
	os.Stderr = w
	desktop.Notify("Pasted", "gpt-4o-mini-transcribe · clean", true)
	desktop.Notify("Nothing heard.", "", true)
	os.Stderr = saved
	w.Close()
	if out, _ := io.ReadAll(r); string(out) != "Pasted\ngpt-4o-mini-transcribe · clean\nNothing heard.\n" {
		t.Fatalf("%q", out)
	}
}

func TestPasteThenEnterWithTheSameTool(t *testing.T) {
	for name, tc := range map[string]struct {
		fail string
		want []string
	}{
		"wtype":   {"", []string{"wtype -M ctrl v -m ctrl", "wtype -k Return"}},
		"ydotool": {"wtype", []string{"ydotool key 29:1 47:1 47:0 29:0", "ydotool key 28:1 28:0"}},
	} {
		t.Run(name, func(t *testing.T) {
			e := testenv.Setup(t)
			t.Setenv("HABLA_FAKE_FAIL", tc.fail)
			if err := desktop.Paste("auto", true); err != nil {
				t.Fatal(err)
			}
			wantLines(t, e.Lines("pasted"), tc.want...)
		})
	}
}
