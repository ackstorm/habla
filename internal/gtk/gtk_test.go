package gtk

import (
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/ackstorm/habla/internal/testenv"
	"github.com/ebitengine/purego"
	"golang.org/x/sys/unix"
)

// GTK must run on the main OS thread, but tests run on other goroutines. init locks the main goroutine to the main
// thread, TestMain runs the tests on another goroutine, and the main goroutine runs what tests send to onMain.
func init() { runtime.LockOSThread() }

var mainQueue = make(chan func())

func TestMain(m *testing.M) {
	done := make(chan int)
	go func() { done <- m.Run() }()
	for {
		select {
		case f := <-mainQueue:
			f()
		case code := <-done:
			os.Exit(code)
		}
	}
}

func onMain(f func()) {
	done := make(chan struct{})
	mainQueue <- func() { defer close(done); f() }
	<-done
}

func TestMissingLibraryNamesThePackage(t *testing.T) {
	err := bind(lib{name: "libhabla-nope.so.0", pkg: "libhabla-nope0"})
	if err == nil || !strings.Contains(err.Error(), "install libhabla-nope0") {
		t.Fatalf("err = %v", err)
	}
	err = bind(lib{name: "libglib-2.0.so.0", pkg: "libglib2.0-0", funcs: map[string]any{"g_nope": new(func())}})
	if err == nil || !strings.Contains(err.Error(), "g_nope") {
		t.Fatalf("err = %v", err)
	}
}

// TestRiskGate is a spike on a private Xvfb: libraries, a POPUP window painted by a Go draw callback, a
// radio menu item activated into a Go callback, an Ayatana indicator with no tray host, Idle from a goroutine, quit.
func TestRiskGate(t *testing.T) {
	if os.Getenv("HABLA_GUI") != "1" {
		t.Skip("set HABLA_GUI=1 to run GTK under Xvfb")
	}
	t.Setenv("DISPLAY", testenv.Xvfb(t))
	t.Setenv("GDK_BACKEND", "x11")
	t.Setenv("WAYLAND_DISPLAY", "")
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path="+t.TempDir()+"/no-bus") // no tray host, no autolaunch
	t.Setenv("NO_AT_BRIDGE", "1")

	var (
		tid                     int
		draws, activates        int
		toggled, idled, timeout bool
		pixel                   [3]byte
		leftover                int
	)
	onMain(func() {
		tid = unix.Gettid()
		if err := Load(); err != nil {
			t.Error(err)
			return
		}
		if err := Init(); err != nil {
			t.Error(err)
			return
		}
		getWindow, pixbufFromWindow, pixbufPixels, unref := readback()

		window := WindowNew(WindowPopup)
		WindowMove(window, 100, 100)
		WindowSetAcceptFocus(window, false)
		area := DrawingAreaNew()
		WidgetSetSizeRequest(area, 64, 32)
		ContainerAdd(window, area)
		ConnectDraw(area, func(cr uintptr) {
			CairoSetSourceRGBA(cr, 1, 0, 0, 1)
			CairoRectangle(cr, 0, 0, float64(WidgetGetAllocatedWidth(area)), float64(WidgetGetAllocatedHeight(area)))
			CairoFill(cr)
			CairoSetSourceRGBA(cr, 1, 1, 1, 1)
			CairoSelectFontFace(cr, "monospace", FontSlantNormal, FontWeightBold)
			CairoSetFontSize(cr, 12)
			var ext TextExtents
			CairoTextExtents(cr, "0:07", &ext)
			if ext.Width <= 0 {
				t.Errorf("text extents = %+v", ext)
			}
			CairoMoveTo(cr, 40, 20)
			CairoShowText(cr, "0:07")
			draws++
		})
		region := CairoRegionCreate()
		WidgetInputShapeCombineRegion(window, region)
		CairoRegionDestroy(region)
		WidgetShowAll(window)

		var area0 Rect
		MonitorGetWorkarea(DisplayGetMonitorAtPoint(DisplayGetDefault(), 10, 10), &area0)
		if area0.Width != 1280 || area0.Height != 800 {
			t.Errorf("workarea = %+v", area0)
		}

		menu := MenuNew()
		a := RadioMenuItemNewWithLabelFromWidget(0, "A")
		b := RadioMenuItemNewWithLabelFromWidget(a, "B")
		MenuShellAppend(menu, a)
		MenuShellAppend(menu, b)
		Connect(b, "toggled", func() { toggled = CheckMenuItemGetActive(b) })
		for range 100 { // many handlers share one C callback
			item := MenuItemNewWithLabel("x")
			Connect(item, "activate", func() { activates++ })
			MenuShellAppend(menu, item)
			MenuItemActivate(item)
		}
		WidgetShowAll(menu)
		MenuItemActivate(b)
		if !toggled || activates != 100 {
			t.Errorf("toggled = %v, activates = %d", toggled, activates)
		}

		indicator := IndicatorNew("habla-test", "habla-idle-symbolic", CategoryApplicationStatus)
		if indicator == 0 {
			t.Error("app_indicator_new returned NULL")
		}
		IndicatorSetIconThemePath(indicator, t.TempDir())
		IndicatorSetStatus(indicator, StatusActive)
		IndicatorSetIconFull(indicator, "habla-recording-symbolic", "Recording")
		IndicatorSetLabel(indicator, "0:07", "0:00")
		IndicatorSetMenu(indicator, menu)
		IndicatorSetSecondaryActivateTarget(indicator, b)

		go Idle(func() { idled = true })

		Timeout(10_000, func() bool { timeout = true; MainQuit(); return false })
		Timeout(20, func() bool {
			if draws == 0 || !idled {
				return true
			}
			pixbuf := pixbufFromWindow(getWindow(window), 0, 0, 64, 32)
			if pixbuf == 0 {
				t.Error("gdk_pixbuf_get_from_window returned NULL")
			} else {
				pixel = *pixbufPixels(pixbuf)
				unref(pixbuf)
			}
			WidgetDestroy(menu) // frees its 101 handlers through the destroy notify
			WidgetDestroy(window)
			MainQuit()
			return false
		})
		Main()
		mu.Lock()
		leftover = len(handles)
		mu.Unlock()
	})

	if tid != os.Getpid() {
		t.Errorf("GTK ran on thread %d, not the main thread %d", tid, os.Getpid())
	}
	if timeout {
		t.Fatalf("the loop did not finish within 10 s: draws = %d, idled = %v", draws, idled)
	}
	if pixel[0] < 200 || pixel[1] > 50 || pixel[2] > 50 {
		t.Errorf("pixel at (0,0) = %v, want red", pixel)
	}
	if leftover > 1 { // the 10 s timeout, still pending when Main returned
		t.Errorf("%d handles left after destroying the widgets", leftover)
	}
	t.Logf("draws = %d, activates = %d, toggled = %v, idled = %v, pixel = %v, layer-shell = %v",
		draws, activates, toggled, idled, pixel, HasLayerShell())
}

// readback binds what the test needs to read the window's pixels back from the X server. It runs on the main
// goroutine, where t.Fatal would strand the test, so it panics.
func readback() (getWindow func(uintptr) uintptr, fromWindow func(uintptr, int32, int32, int32, int32) uintptr,
	pixels func(uintptr) *[3]byte, unref func(uintptr)) {
	for _, l := range []struct {
		name  string
		funcs map[string]any
	}{
		{"libgtk-3.so.0", map[string]any{"gtk_widget_get_window": &getWindow}},
		{"libgdk-3.so.0", map[string]any{"gdk_pixbuf_get_from_window": &fromWindow}},
		{"libgdk_pixbuf-2.0.so.0", map[string]any{"gdk_pixbuf_get_pixels": &pixels}},
		{"libgobject-2.0.so.0", map[string]any{"g_object_unref": &unref}},
	} {
		h, err := purego.Dlopen(l.name, purego.RTLD_NOW)
		if err != nil {
			panic(err)
		}
		for name, fn := range l.funcs {
			purego.RegisterLibFunc(fn, h, name)
		}
	}
	return
}
