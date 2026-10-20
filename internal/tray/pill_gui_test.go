package tray

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ackstorm/habla/internal/desktop"
	"github.com/ackstorm/habla/internal/rundir"
	"github.com/ackstorm/habla/internal/testenv"
	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"
)

func TestPasteAndEnterX11GUI(t *testing.T) {
	guiEnv(t, "")
	x := newX(t)
	x.activate(t, 100, 100, 400, 300)
	w := desktop.ActiveWindow(x.conn, x.root)
	class := []byte("mate-terminal\x00Mate-terminal\x00")
	if err := xproto.ChangePropertyChecked(x.conn, xproto.PropModeReplace, w, xproto.AtomWmClass,
		xproto.AtomString, 8, uint32(len(class)), class).Check(); err != nil {
		t.Fatal(err)
	}
	if err := xproto.ChangeWindowAttributesChecked(x.conn, w, xproto.CwEventMask,
		[]uint32{xproto.EventMaskKeyPress | xproto.EventMaskKeyRelease}).Check(); err != nil {
		t.Fatal(err)
	}
	if err := xproto.SetInputFocusChecked(x.conn, xproto.InputFocusPointerRoot, w, xproto.TimeCurrentTime).Check(); err != nil {
		t.Fatal(err)
	}
	codes, err := desktop.Keycodes(x.conn, xproto.Setup(x.conn))
	if err != nil {
		t.Fatal(err)
	}
	type key struct {
		press bool
		code  xproto.Keycode
		state uint16
	}
	for _, enter := range []bool{false, true} {
		if err := desktop.Paste("auto", enter); err != nil {
			t.Fatal(err)
		}
		v, ret := codes[0x76], codes[0xff0d]
		want := []key{{true, v, xproto.ModMaskControl | xproto.ModMaskShift}, {false, v, xproto.ModMaskControl | xproto.ModMaskShift}}
		if enter {
			want = append(want, key{true, ret, 0}, key{false, ret, 0})
		}
		var got []key
		testenv.WaitFor(t, "paste key events", func() bool {
			for {
				e, err := x.conn.PollForEvent()
				if err != nil {
					t.Fatal(err)
				}
				if e == nil {
					break
				}
				switch e := e.(type) {
				case xproto.KeyPressEvent:
					if e.Detail == v || e.Detail == ret {
						got = append(got, key{true, e.Detail, e.State})
					}
				case xproto.KeyReleaseEvent:
					if e.Detail == v || e.Detail == ret {
						got = append(got, key{false, e.Detail, e.State})
					}
				}
			}
			return len(got) >= len(want)
		})
		if !slices.Equal(got, want) {
			t.Fatalf("enter=%t: %v, want %v", enter, got, want)
		}
	}
}

// xserver is a test connection to guiEnv's Xvfb. GDK sees one monitor there: it lists only RandR monitors with a
// connected output, Xvfb has one, so `xrandr --setmonitor` monitors are skipped. TestPlacePill covers two monitors.
type xserver struct {
	conn *xgb.Conn
	root xproto.Window
}

// newX connects. Without a window manager the work area is the monitor: GDK reads _NET_WORKAREA only from a WM
// that lists it in _NET_SUPPORTED, and only for the primary monitor (Xvfb has none). TestPlacePill covers work areas.
func newX(t *testing.T) *xserver {
	t.Helper()
	conn, err := xgb.NewConn()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(conn.Close)
	return &xserver{conn, xproto.Setup(conn).DefaultScreen(conn).Root}
}

func (x *xserver) atom(t *testing.T, name string) xproto.Atom {
	t.Helper()
	reply, err := xproto.InternAtom(x.conn, false, uint16(len(name)), name).Reply()
	if err != nil {
		t.Fatal(err)
	}
	return reply.Atom
}

// activate maps a window and makes it _NET_ACTIVE_WINDOW. Xvfb has no window manager (so no xdotool
// windowactivate): the test sets the root property the way one would.
func (x *xserver) activate(t *testing.T, left, top, width, height int) {
	t.Helper()
	w, err := xproto.NewWindowId(x.conn)
	if err != nil {
		t.Fatal(err)
	}
	screen := xproto.Setup(x.conn).DefaultScreen(x.conn)
	if err := xproto.CreateWindowChecked(x.conn, screen.RootDepth, w, x.root, int16(left), int16(top), uint16(width),
		uint16(height), 0, xproto.WindowClassInputOutput, screen.RootVisual, 0, nil).Check(); err != nil {
		t.Fatal(err)
	}
	xproto.MapWindow(x.conn, w)
	if err := xproto.ChangePropertyChecked(x.conn, xproto.PropModeReplace, x.root, x.atom(t, "_NET_ACTIVE_WINDOW"),
		xproto.AtomWindow, 32, 1, word(uint32(w))).Check(); err != nil {
		t.Fatal(err)
	}
}

func word(v uint32) []byte {
	b := make([]byte, 4)
	xgb.Put32(b, v)
	return b
}

// pill is the geometry of the mapped override-redirect window, if there is one.
func (x *xserver) pill(t *testing.T) (*xproto.GetGeometryReply, xproto.Window) {
	t.Helper()
	tree, err := xproto.QueryTree(x.conn, x.root).Reply()
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range tree.Children {
		attrs, err := xproto.GetWindowAttributes(x.conn, w).Reply()
		if err != nil || !attrs.OverrideRedirect || attrs.MapState != xproto.MapStateViewable {
			continue
		}
		if g, err := xproto.GetGeometry(x.conn, xproto.Drawable(w)).Reply(); err == nil {
			return g, w
		}
	}
	return nil, 0
}

// pixel is the window's colour at (px, py) as r, g, b.
func (x *xserver) pixel(t *testing.T, w xproto.Window, px, py int) [3]byte {
	t.Helper()
	img, err := xproto.GetImage(x.conn, xproto.ImageFormatZPixmap, xproto.Drawable(w), int16(px), int16(py), 1, 1,
		^uint32(0)).Reply()
	if err != nil || len(img.Data) < 3 {
		t.Fatalf("GetImage: %v", err)
	}
	return [3]byte{img.Data[2], img.Data[1], img.Data[0]} // BGRX
}

// wantPill waits for the pill and checks it is centred on the monitor, MARGIN above its bottom, all
// times the GDK scale, and painted: the white pill with the rose dot.
func (x *xserver) wantPill(t *testing.T, scale int) {
	t.Helper()
	var g *xproto.GetGeometryReply
	var w xproto.Window
	testenv.WaitFor(t, "the pill", func() bool { g, w = x.pill(t); return g != nil })
	centre, bottom := int(g.X)+int(g.Width)/2, int(g.Y)+int(g.Height)
	if centre < 640-scale || centre > 640+scale || bottom != 800-48*scale || int(g.Height) != 54*scale ||
		int(g.Width) != 604*scale {
		t.Fatalf("pill at %dx%d+%d+%d: centre %d, bottom %d; want centre 640, bottom %d", g.Width, g.Height, g.X, g.Y,
			centre, bottom, 800-48*scale)
	}
	time.Sleep(300 * time.Millisecond) // a few frames
	if bg := x.pixel(t, w, int(g.Width)-10*scale, int(g.Height)/2); bg != [3]byte{255, 255, 255} {
		t.Errorf("background %v, want the pill's white surface", bg)
	}
	if edge := x.pixel(t, w, int(g.Width)/2, 4*scale); edge != [3]byte{181, 196, 211} {
		t.Errorf("outline %v, want a pale edge around the white body", edge)
	}
	for _, sep := range []int{176, 504} {
		pixel := x.pixel(t, w, sep*scale, 20*scale)
		if pixel[0] < 230 || pixel[0] > 245 || pixel[1] < 235 || pixel[2] < 240 {
			t.Errorf("separator %v, want a very subtle line", pixel)
		}
		for _, side := range []int{sep - 5, sep + 5} {
			if got := x.pixel(t, w, side*scale, 20*scale); got != [3]byte{255, 255, 255} {
				t.Errorf("separator margin %v, want white space on both sides", got)
			}
		}
	}
	if dot := x.pixel(t, w, 29*scale, int(g.Height)/2); dot[0] < 150 || int(dot[0])-int(dot[1]) < 20 {
		t.Errorf("dot %v, want red", dot)
	}
}

func (x *xserver) wantNoPill(t *testing.T) {
	t.Helper()
	testenv.WaitFor(t, "the pill to go", func() bool { g, _ := x.pill(t); return g == nil })
}

func listenCLI(t *testing.T, e *testenv.Env, flag string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, filepath.Join(e.Bin, "habla"), "listen", flag).CombinedOutput(); err != nil {
		t.Fatalf("habla listen %s: %v\n%s", flag, err, out)
	}
}

// startPillTray is startTray, with the tray's output in the test log on failure.
func startPillTray(t *testing.T, e *testenv.Env, privateBus bool) string {
	t.Helper()
	log, _ := startTray(t, e, privateBus)
	t.Cleanup(func() {
		if data, _ := os.ReadFile(log); t.Failed() {
			t.Logf("tray output:\n%s", data)
		}
	})
	return log
}

func TestPillGUI(t *testing.T) {
	e, _ := guiEnv(t, "")
	x := newX(t)
	x.activate(t, 600, 200, 400, 300)
	log := startPillTray(t, e, true)
	waitLog(t, log, "No tray to show Habla's icon") // the hotkey is grabbed by then
	xdo(t, "keydown", "super+space")
	testenv.WaitFor(t, "recording", recording)
	x.wantPill(t, 1)
	testenv.WaitFor(t, "the fake recorder to finish writing", func() bool { return len(e.Lines("recorded")) > 0 })
	path := filepath.Join(rundir.Dir(), "current.wav")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	paintAudio := func(sample int16) {
		t.Helper()
		for i := 44; i+1 < len(data); i += 2 {
			binary.LittleEndian.PutUint16(data[i:], uint16(sample))
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	_, window := x.pill(t)
	paintAudio(-32768)
	testenv.WaitFor(t, "a tall red near-clipping bar", func() bool {
		pixel := x.pixel(t, window, 52, 17)
		return pixel[0] > 200 && int(pixel[0])-int(pixel[1]) > 70
	})
	paintAudio(0)
	testenv.WaitFor(t, "a one-pixel idle line", func() bool {
		return x.pixel(t, window, 52, 26) == [3]byte{255, 255, 255} &&
			x.pixel(t, window, 52, 28) == [3]byte{255, 255, 255}
	})
	for _, px := range []int{52, 55, 150, 164} {
		if pixel := x.pixel(t, window, px, 27); pixel != [3]byte{170, 166, 255} {
			t.Errorf("idle line at %d: %v, want a continuous lavender hairline", px, pixel)
		}
	}
	for _, sample := range []int16{104, 184, 327} { // -50, -45 and -40 dBFS: low residual input stays flat
		paintAudio(sample)
		time.Sleep(200 * time.Millisecond)
		for _, py := range []int{26, 28} {
			if pixel := x.pixel(t, window, 52, py); pixel != [3]byte{255, 255, 255} {
				t.Errorf("low microphone input %d at y=%d: %v, want a one-pixel idle line", sample, py, pixel)
			}
		}
	}
	paintAudio(1036) // about -30 dBFS: soft speech should fill 50–60% of the 24 px height
	testenv.WaitFor(t, "soft speech at roughly half height", func() bool {
		pixel := x.pixel(t, window, 52, 21)
		return pixel[0] < 230 && pixel[1] > 150 && pixel[2] > 200
	})
	if pixel := x.pixel(t, window, 52, 19); pixel != [3]byte{255, 255, 255} {
		t.Errorf("soft speech %v, want headroom above the bar", pixel)
	}
	paintAudio(3277) // about -20 dBFS
	testenv.WaitFor(t, "amber speech visibly above the quiet baseline", func() bool {
		pixel := x.pixel(t, window, 52, 23)
		return pixel == [3]byte{255, 208, 124}
	})
	paintAudio(5827) // about -15 dBFS
	testenv.WaitFor(t, "a tall bar for ordinary speech", func() bool {
		pixel := x.pixel(t, window, 52, 19)
		return pixel == [3]byte{255, 208, 124}
	})
	paintAudio(9235) // about -11 dBFS: strong speech reaches red well before clipping
	testenv.WaitFor(t, "red for strong speech above the middle range", func() bool {
		pixel := x.pixel(t, window, 52, 18)
		return pixel == [3]byte{255, 119, 125}
	})
	if pixel := x.pixel(t, window, 52, 19); pixel != [3]byte{255, 119, 125} {
		t.Errorf("speech %v, want a red bar at least 16 px tall", pixel)
	}
	xdo(t, "keyup", "super+space")
	x.wantNoPill(t)
	testenv.WaitFor(t, "idle", idle)
}

// Under GDK_SCALE=2 the pill is twice the size, still centred above the work area: X reports device pixels, GDK app
// pixels. The recording starts outside the tray (habla listen), before it: its first refresh shows the pill.
func TestPillScaledGUI(t *testing.T) {
	e, _ := guiEnv(t, "")
	t.Setenv("GDK_SCALE", "2")
	x := newX(t)
	x.activate(t, 600, 200, 400, 300)
	listenCLI(t, e, "--start")
	startPillTray(t, e, true)
	x.wantPill(t, 2)
	listenCLI(t, e, "--cancel")
	x.wantNoPill(t)
}

func TestPillStaysThroughTranscriptionAndCopyGUI(t *testing.T) {
	e, _ := guiEnv(t, "")
	r := testenv.DefaultReplies
	r.HoldSTT = make(chan struct{})
	r.HoldChat = make(chan struct{})
	g := testenv.NewGateway(t, r)
	e.WriteConfig(t, g, "")
	defer close(r.HoldChat)
	defer close(r.HoldSTT)
	gate := filepath.Join(e.Dir, "copy-gate")
	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HABLA_FAKE_COPY_GATE", gate)
	x := newX(t)
	x.activate(t, 600, 200, 400, 300)
	startPillTray(t, e, true)
	listenCLI(t, e, "--start")
	x.wantPill(t, 1)
	before, window := x.pill(t)
	wantActivity := func(color [3]byte) {
		t.Helper()
		testenv.WaitFor(t, "processing activity in the expected phase color", func() bool {
			img, err := xproto.GetImage(x.conn, xproto.ImageFormatZPixmap, xproto.Drawable(window), 51, 24, 115, 6, ^uint32(0)).Reply()
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i+3 < len(img.Data); i += 4 {
				if [3]byte{img.Data[i+2], img.Data[i+1], img.Data[i]} == color {
					return true
				}
			}
			return false
		})
	}
	listenCLI(t, e, "--stop")
	testenv.WaitFor(t, "transcribing", func() bool { return rundir.ReadState() == "busy" })
	wantActivity([3]byte{139, 221, 219})
	cancel, err := xproto.GetImage(x.conn, xproto.ImageFormatZPixmap, xproto.Drawable(window), 514, 11, 52, 32, ^uint32(0)).Reply()
	if err != nil {
		t.Fatal(err)
	}
	cancelInk := false
	for i := 0; i+3 < len(cancel.Data); i += 4 {
		cancelInk = cancelInk || cancel.Data[i] < 180
	}
	if !cancelInk {
		t.Fatal("Escape must remain visible while transcription can be cancelled")
	}
	time.Sleep(100 * time.Millisecond)
	if after, current := x.pill(t); after == nil || current != window || after.X != before.X || after.Y != before.Y ||
		after.Width != before.Width || after.Height != before.Height {
		t.Fatalf("the recording pill disappeared or moved while transcribing: before %+v (window %d), after %+v (window %d)", before, window, after, current)
	}
	timer, err := xproto.GetImage(x.conn, xproto.ImageFormatZPixmap, xproto.Drawable(window), 451, 15, 42, 24, ^uint32(0)).Reply()
	if err != nil {
		t.Fatal(err)
	}
	ink := false
	for i := 0; i+3 < len(timer.Data); i += 4 {
		ink = ink || timer.Data[i] < 180
	}
	if !ink {
		t.Fatal("the duration must be visible immediately before the cancel separator")
	}
	time.Sleep(1100 * time.Millisecond)
	frozen, err := xproto.GetImage(x.conn, xproto.ImageFormatZPixmap, xproto.Drawable(window), 451, 15, 42, 24, ^uint32(0)).Reply()
	if err != nil || !bytes.Equal(timer.Data, frozen.Data) {
		t.Fatal("the recorded duration must freeze during transcription", err)
	}
	r.HoldSTT <- struct{}{}
	testenv.WaitFor(t, "transform", func() bool { return rundir.ReadState() == "transform" })
	wantActivity([3]byte{243, 205, 131})
	refining, err := xproto.GetImage(x.conn, xproto.ImageFormatZPixmap, xproto.Drawable(window), 451, 15, 42, 24, ^uint32(0)).Reply()
	if err != nil || !bytes.Equal(timer.Data, refining.Data) {
		t.Fatal("the recorded duration must remain unchanged during refinement", err)
	}
	// Release transform without closing it again during transform.
	r.HoldChat <- struct{}{}
	testenv.WaitFor(t, "the clipboard operation", func() bool { return len(e.Lines("copy-started")) > 0 })
	if rundir.ReadState() != "copying" {
		t.Fatalf("copy is not finished, state = %q", rundir.ReadState())
	}
	wantActivity([3]byte{243, 205, 131}) // delivery retains transform's appearance
	testenv.WaitFor(t, "the unavailable cancel instruction to disappear", func() bool {
		img, err := xproto.GetImage(x.conn, xproto.ImageFormatZPixmap, xproto.Drawable(window), 514, 11, 52, 32, ^uint32(0)).Reply()
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i+3 < len(img.Data); i += 4 {
			if img.Data[i] != 255 || img.Data[i+1] != 255 || img.Data[i+2] != 255 {
				return false
			}
		}
		return true
	})
	if after, current := x.pill(t); after == nil || current != window || after.X != before.X || after.Y != before.Y ||
		after.Width != before.Width || after.Height != before.Height {
		t.Fatalf("the pill disappeared or moved before clipboard completion: before %+v (window %d), after %+v (window %d)", before, window, after, current)
	}
	if err := os.Remove(gate); err != nil {
		t.Fatal(err)
	}
	testenv.WaitFor(t, "clipboard delivery", func() bool { return len(e.Lines("copied")) == 1 })
	x.wantNoPill(t)
}

func TestCopyFailureKeepsAnErrorPillGUI(t *testing.T) {
	e, _ := guiEnv(t, "")
	t.Setenv("HABLA_FAKE_FAIL", "xclip")
	x := newX(t)
	startPillTray(t, e, true)
	listenCLI(t, e, "--start")
	x.wantPill(t, 1)
	listenCLI(t, e, "--stop")
	testenv.WaitFor(t, "copy failure", func() bool { return rundir.ReadState() == "copy_failed" })
	testenv.WaitFor(t, "the error pill", func() bool {
		geometry, window := x.pill(t)
		if geometry == nil {
			return false
		}
		pixel := x.pixel(t, window, 52, 27)
		return pixel[0] > 200 && int(pixel[0])-int(pixel[1]) > 70
	})
	if len(e.Lines("copied")) != 0 {
		t.Fatal("failed copy must not report clipboard success")
	}
	listenCLI(t, e, "--start")
	testenv.WaitFor(t, "the new recorder", func() bool { return len(e.Lines("recorded")) == 2 })
	listenCLI(t, e, "--cancel")
	x.wantNoPill(t)
}

// On Wayland without layer-shell (GNOME), a critical notification stands in for the pill and is closed when the
// recording ends. GTK runs on Xvfb here, where gtk_layer_is_supported is false, as on GNOME.
func TestPillNoticeWithoutLayerShellGUI(t *testing.T) {
	e, _ := guiEnv(t, "")
	t.Setenv("WAYLAND_DISPLAY", "wayland-habla")
	t.Setenv("XDG_SESSION_TYPE", "wayland")
	t.Setenv("XDG_CURRENT_DESKTOP", "") // no portal hotkey
	fakeBus(t, e, "none")
	x := newX(t)
	startPillTray(t, e, false)
	listenCLI(t, e, "--start")
	recordingNotes := func() []string {
		return slices.DeleteFunc(e.Lines("notifications"), func(l string) bool { return strings.Contains(l, "No tray") })
	}
	testenv.WaitFor(t, "the notification", func() bool { return len(recordingNotes()) == 1 })
	listenCLI(t, e, "--cancel")
	testenv.WaitFor(t, "the notification to close", func() bool { return len(recordingNotes()) == 2 })
	want := []string{"Notify 1 Listening|Stop or discard from the tray urgency=2", "Close 1"}
	if got := recordingNotes(); !slices.Equal(got, want) {
		t.Errorf("notifications %q, want %q", got, want)
	}
	if g, _ := x.pill(t); g != nil {
		t.Errorf("a pill window at %+v", g)
	}
}
