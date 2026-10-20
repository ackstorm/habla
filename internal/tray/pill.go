package tray

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ackstorm/habla/internal/desktop"
	"github.com/ackstorm/habla/internal/gtk"
	"github.com/ackstorm/habla/internal/rundir"
	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"
)

// The recording pill: a red dot, sound-level bars, the elapsed time and how to stop
// or discard, near the bottom of the active window's monitor. Clicks pass through it and it never takes focus, so the
// paste still lands in the window the user was typing in. Drawn with cairo.
const (
	bars       = 28
	frameMS    = 33 // about 30 fps, only while shown
	pillHeight = 54 // 46 px body and 4 px shadow space on each side
	meterWidth = 115
	pillTextX  = 51 + meterWidth + 21
)

// recordingView follows a dictation through recording, processing and clipboard delivery.
type recordingView interface {
	show(phase string, hint []Segment)
	hide()
}

type pill struct {
	window, area, measure uintptr // measure: a cairo context for text widths before the first draw
	layer                 bool
	phase                 string
	hint                  []Segment
	levels                []float64
	lastFrame             time.Time
	elapsed               string
	since, until          time.Time // the processing phase's start (StateWritten) and, once delivery begins, its end
	width                 int32
	ticker                uint32 // the frame timeout while shown; 0 when hidden
}

// newPill builds the pill: an override-redirect window on X11, a layer-shell overlay on Wayland where the compositor
// has it (KDE, sway, Hyprland), else (GNOME) the notice. nil, with the tray icon alone, without an RGBA visual.
func newPill() recordingView {
	layer := desktop.Wayland() && gtk.HasLayerShell() && gtk.LayerIsSupported()
	if desktop.Wayland() && !layer {
		return &notice{}
	}
	visual := gtk.ScreenGetRGBAVisual(gtk.ScreenGetDefault())
	if visual == 0 {
		fmt.Fprintln(os.Stderr, "habla tray: no recording pill: the screen has no RGBA visual")
		return nil
	}
	p := &pill{layer: layer, measure: gtk.CairoCreate(gtk.CairoImageSurfaceCreate(gtk.FormatARGB32, 1, 1))}
	kind := int32(gtk.WindowPopup)
	if layer {
		kind = gtk.WindowToplevel
	}
	p.window = gtk.WindowNew(kind)
	gtk.WindowSetAcceptFocus(p.window, false)
	gtk.WidgetSetVisual(p.window, visual) // round ends
	gtk.WidgetSetAppPaintable(p.window, true)
	if layer { // before realize; the output is left unset: the compositor picks the focused one
		gtk.LayerInitForWindow(p.window)
		gtk.LayerSetNamespace(p.window, "habla")
		gtk.LayerSetLayer(p.window, gtk.LayerOverlay)
		gtk.LayerSetAnchor(p.window, gtk.EdgeBottom, true) // only the bottom: centred horizontally
		gtk.LayerSetMargin(p.window, gtk.EdgeBottom, pillMargin)
		gtk.LayerSetKeyboardMode(p.window, gtk.KeyboardModeNone)
	}
	p.area = gtk.DrawingAreaNew()
	gtk.ContainerAdd(p.window, p.area)
	gtk.ConnectDraw(p.area, p.draw)
	region := gtk.CairoRegionCreate() // empty: clicks pass through
	gtk.WidgetInputShapeCombineRegion(p.window, region)
	gtk.CairoRegionDestroy(region)
	gtk.WidgetShow(p.area)
	return p
}

func (p *pill) show(phase string, hint []Segment) {
	visible := p.ticker != 0
	if rundir.Processing(phase) && phase != p.phase {
		p.since, p.until = StateWritten(), time.Time{}
	}
	p.phase, p.hint = phase, hint
	if phase == "recording" {
		p.levels = nil // a new recording never inherits the previous one's peaks
	} else if p.elapsed == "" {
		p.elapsed = "--:--" // the tray may have started after recording ended
	}
	if !visible || phase == "recording" {
		p.width = 0
	}
	p.frame()
	if !p.layer && (!visible || phase == "recording") {
		p.place()
	}
	gtk.WidgetShow(p.window)
	if p.ticker == 0 {
		p.ticker = gtk.Timeout(frameMS, p.frame)
	}
}

func (p *pill) hide() {
	if id := p.ticker; id != 0 {
		p.ticker = 0
		gtk.SourceRemove(id)
	}
	gtk.WidgetHide(p.window)
}

// frame reads the levels and the timer, fits the window to them and redraws.
func (p *pill) frame() bool {
	if p.phase == "recording" {
		now := time.Now()
		levels := Levels(filepath.Join(rundir.Dir(), "current.wav"), bars, 0.24)
		if len(p.levels) == len(levels) {
			decay := math.Exp(-now.Sub(p.lastFrame).Seconds() / 0.14)
			for i, level := range levels {
				if level < p.levels[i] { // immediate attack, gentle fall; silence adds no artificial motion
					levels[i] += (p.levels[i] - level) * decay
				}
			}
		}
		p.levels, p.lastFrame = levels, now
		p.elapsed = ElapsedLabel(RecordingSeconds())
	} else if rundir.Processing(p.phase) && p.until.IsZero() {
		if written := StateWritten(); written.After(p.since) { // copying or pasting: the phase has ended
			p.until = written
		}
	}
	w := int32(583)
	for _, phase := range []string{"recording", "busy", "transform", "copy_failed"} {
		q := *p
		q.phase = phase
		title, subtitle, _ := q.labels()
		// Reserve all phases before showing the window, including unusually wide system fonts.
		if rundir.Processing(phase) {
			title += " (00.0s)" // the phase timer, drawn after the title
		}
		textWidth := max(pillText(p.measure, false, title, 0, 0, 13, "sans-serif", true),
			pillText(p.measure, false, subtitle, 0, 0, 11, "sans-serif", false))
		timerWidth := pillText(p.measure, false, p.elapsed, 0, 0, 11, "monospace", false)
		w = max(w, int32(math.Ceil(pillTextX+textWidth+10+max(35, timerWidth)+90)))
	}
	w = max(w, p.width)
	if w != p.width {
		p.width = w
		gtk.WidgetSetSizeRequest(p.area, w, pillHeight)
		gtk.WindowResize(p.window, w, pillHeight) // a window only grows by itself
	}
	gtk.WidgetQueueDraw(p.area)
	return true
}

// place moves the X11 pill onto the active window's monitor (PlacePill). X reports device pixels, GDK app pixels.
// No monitors (a closed lid, mid-hotplug): no move, and no NULL monitor for the scale.
func (p *pill) place() {
	display := gtk.DisplayGetDefault()
	if gtk.DisplayGetNMonitors(display) == 0 {
		return
	}
	var monitors, workareas []Rect
	for i := range gtk.DisplayGetNMonitors(display) {
		var g, w gtk.Rect
		gtk.MonitorGetGeometry(gtk.DisplayGetMonitor(display, i), &g)
		gtk.MonitorGetWorkarea(gtk.DisplayGetMonitor(display, i), &w)
		monitors = append(monitors, Rect{int(g.X), int(g.Y), int(g.Width), int(g.Height)})
		workareas = append(workareas, Rect{int(w.X), int(w.Y), int(w.Width), int(w.Height)})
	}
	scale := int(gtk.MonitorGetScaleFactor(gtk.DisplayGetMonitor(display, 0))) // one scale for all on X11
	active, pointer := focus()
	if active != nil {
		active = &Rect{active.X / scale, active.Y / scale, active.Width / scale, active.Height / scale}
	}
	x, y := PlacePill(monitors, workareas, active, Point{pointer.X / scale, pointer.Y / scale},
		Size{int(p.width), pillHeight})
	gtk.WindowMove(p.window, int32(x), int32(y))
}

// focus is the active window (nil without one, or when it is the desktop) and the pointer, in X device pixels.
func focus() (*Rect, Point) {
	conn, err := xgb.NewConn()
	if err != nil {
		return nil, Point{}
	}
	defer conn.Close()
	root := xproto.Setup(conn).DefaultScreen(conn).Root
	var pointer Point
	if reply, err := xproto.QueryPointer(conn, root).Reply(); err == nil {
		pointer = Point{int(reply.RootX), int(reply.RootY)}
	}
	window := desktop.ActiveWindow(conn, root)
	if window == 0 || isDesktop(conn, window) {
		return nil, pointer
	}
	g, err := xproto.GetGeometry(conn, xproto.Drawable(window)).Reply()
	if err != nil {
		return nil, pointer
	}
	at, err := xproto.TranslateCoordinates(conn, window, root, 0, 0).Reply()
	if err != nil {
		return nil, pointer
	}
	return &Rect{int(at.DstX), int(at.DstY), int(g.Width), int(g.Height)}, pointer
}

// isDesktop reports a _NET_WM_WINDOW_TYPE_DESKTOP window (MATE's caja, say, which spans every monitor).
func isDesktop(conn *xgb.Conn, window xproto.Window) bool {
	var atoms [2]xproto.Atom
	for i, name := range []string{"_NET_WM_WINDOW_TYPE", "_NET_WM_WINDOW_TYPE_DESKTOP"} {
		reply, err := xproto.InternAtom(conn, true, uint16(len(name)), name).Reply()
		if err != nil || reply.Atom == 0 {
			return false
		}
		atoms[i] = reply.Atom
	}
	kind, err := xproto.GetProperty(conn, false, window, atoms[0], xproto.AtomAtom, 0, 1).Reply()
	return err == nil && len(kind.Value) >= 4 && xproto.Atom(xgb.Get32(kind.Value)) == atoms[1]
}

// labels keeps the visual instructions consistent with the existing shortcut capabilities.
func (p *pill) labels() (title, subtitle, cancel string) {
	switch p.phase {
	case "recording":
		title, subtitle, cancel = "Listening", "Stop from the tray", "Tray"
		if len(p.hint) > 0 && strings.HasPrefix(p.hint[0].Text, "Release") {
			subtitle = "Release to transcribe…"
		}
	case "busy":
		title, subtitle = "Transcribing", "Turning your words into text…"
	case "transform":
		title, subtitle = "Refining text", "Corrections and style"
	case "copy_failed":
		title, subtitle = "Copy failed", "See notification"
	}
	for _, segment := range p.hint {
		if segment.Key && segment.Text == "Esc" {
			cancel = "ESC"
		}
	}
	return
}

// draw paints the Paper light body with Orbit phase colours.
func (p *pill) draw(cr uintptr) {
	w, h := float64(gtk.WidgetGetAllocatedWidth(p.area)), float64(pillHeight)
	middle := h / 2
	gtk.CairoSetOperator(cr, gtk.OperatorSource)
	gtk.CairoSetSourceRGBA(cr, 0, 0, 0, 0)
	composited := gtk.ScreenIsComposited(gtk.ScreenGetDefault())
	if !composited {
		gtk.CairoSetSourceRGBA(cr, 1, 1, 1, 1)
	}
	gtk.CairoPaint(cr)
	gtk.CairoSetOperator(cr, gtk.OperatorOver)
	if composited {
		for spread := 4.0; spread >= 1; spread-- {
			gtk.CairoSetSourceRGBA(cr, 0, 0, 0, 0.035)
			roundRect(cr, 4-spread, 5-spread/2, w-8+2*spread, h-8+spread, 12+spread)
		}
	}
	gtk.CairoSetSourceRGBA(cr, 181/255.0, 196/255.0, 211/255.0, 1)
	roundRect(cr, 4, 4, w-8, h-8, 12)
	gtk.CairoSetSourceRGBA(cr, 1, 1, 1, 1)
	roundRect(cr, 5, 5, w-10, h-10, 11)

	accent, ink := [3]float64{170, 166, 255}, [3]float64{101, 81, 174}
	switch p.phase {
	case "busy":
		accent, ink = [3]float64{139, 221, 219}, [3]float64{22, 120, 117}
	case "transform":
		accent, ink = [3]float64{243, 205, 131}, [3]float64{137, 93, 20}
	case "copy_failed":
		accent, ink = [3]float64{251, 157, 181}, [3]float64{163, 65, 96}
	}
	gtk.CairoSetSourceRGBA(cr, accent[0]/255, accent[1]/255, accent[2]/255, 0.23)
	roundRect(cr, 17, middle-12, 24, 24, 7)
	angle := float64(time.Now().UnixMilli()%1800) / 1800 * 2 * math.Pi
	switch p.phase {
	case "recording":
		gtk.CairoSetSourceRGBA(cr, 163/255.0, 65/255.0, 96/255.0, 1)
		circle(cr, 29, middle, 4.5)
		gtk.CairoSetSourceRGBA(cr, 251/255.0, 157/255.0, 181/255.0, 0.55+0.45*(1+math.Sin(angle))/2)
		circle(cr, 29, middle, 3.5)
		gtk.CairoSetSourceRGBA(cr, accent[0]/255, accent[1]/255, accent[2]/255, 1)
		gtk.CairoRectangle(cr, 51, middle, meterWidth, 1)
		gtk.CairoFill(cr)
		for i, level := range p.levels {
			color := accent
			switch {
			case level >= 0.76: // -12 dBFS: strong speech
				color = [3]float64{255, 119, 125}
			case level >= 0.6: // -20 dBFS
				color = [3]float64{255, 208, 124}
			}
			gtk.CairoSetSourceRGBA(cr, color[0]/255, color[1]/255, color[2]/255, 1)
			visual := math.Sqrt(max(0, (level-0.2)/0.8)) // Visual floor at -40 dBFS; -30 dBFS speech fills half the height.
			height := math.Round(1 + visual*23)
			if height > 1 {
				roundRect(cr, 51+float64(i)*(meterWidth-3)/(bars-1), middle-height/2, 3, height, min(1.5, height/2))
			}
		}
	case "busy":
		for i := range 8 {
			a := angle*2 + float64(i)*math.Pi/4
			gtk.CairoSetSourceRGBA(cr, ink[0]/255, ink[1]/255, ink[2]/255, 0.2+0.8*float64(i)/7)
			circle(cr, 29+6*math.Cos(a), middle+6*math.Sin(a), 1.3)
		}
	case "transform":
		gtk.CairoSetSourceRGBA(cr, ink[0]/255, ink[1]/255, ink[2]/255, 0.5+0.5*(1+math.Sin(angle))/2)
		for i := range 8 {
			a := float64(i)*math.Pi/4 - math.Pi/2
			r := 8.0
			if i%2 != 0 {
				r = 2.5
			}
			x, y := 29+r*math.Cos(a), middle+r*math.Sin(a)
			if i == 0 {
				gtk.CairoMoveTo(cr, x, y)
			} else {
				gtk.CairoLineTo(cr, x, y)
			}
		}
		gtk.CairoFill(cr)
	case "copy_failed":
		gtk.CairoSetSourceRGBA(cr, ink[0]/255, ink[1]/255, ink[2]/255, 1)
		circle(cr, 29, middle, 4.5)
	}
	if p.phase != "recording" {
		gtk.CairoSetSourceRGBA(cr, accent[0]/255, accent[1]/255, accent[2]/255, 0.22)
		roundRect(cr, 51, middle-2, meterWidth, 4, 2)
		gtk.CairoSetSourceRGBA(cr, accent[0]/255, accent[1]/255, accent[2]/255, 1)
		sweep := float64(time.Now().UnixMilli()%1300) / 1300 * 2 * math.Pi // 650 ms in each direction
		x, width := 51+(meterWidth-20)/2.0*(1+math.Sin(sweep)), 20.0
		if p.phase == "copy_failed" {
			x, width = 51, meterWidth
		}
		roundRect(cr, x, middle-2, width, 4, 2)
	}

	gtk.CairoSetSourceRGBA(cr, 181/255.0, 196/255.0, 211/255.0, 0.25)
	for _, x := range []float64{pillTextX - 11, w - 80} {
		gtk.CairoRectangle(cr, x, middle-13, 1, 26)
		gtk.CairoFill(cr)
	}
	title, subtitle, cancel := p.labels()
	gtk.CairoSetSourceRGBA(cr, ink[0]/255, ink[1]/255, ink[2]/255, 1)
	titleWidth := pillText(cr, true, title, pillTextX, middle-3, 13, "sans-serif", true)
	gtk.CairoSetSourceRGBA(cr, 82/255.0, 97/255.0, 119/255.0, 1)
	if rundir.Processing(p.phase) {
		pillText(cr, true, PhaseLabel(p.since, p.until, time.Now()), pillTextX+titleWidth+6, middle-3, 11, "monospace", false)
	}
	pillText(cr, true, subtitle, pillTextX, middle+12, 11, "sans-serif", false)
	timerWidth := pillText(cr, false, p.elapsed, 0, 0, 11, "monospace", false)
	pillText(cr, true, p.elapsed, w-90-timerWidth, middle+4, 11, "monospace", false)
	if cancel != "" {
		keyWidth := pillText(cr, false, cancel, 0, 0, 10, "monospace", false)
		center := w - 43
		gtk.CairoSetSourceRGBA(cr, 181/255.0, 196/255.0, 211/255.0, 0.65)
		roundRect(cr, center-keyWidth/2-5, middle-15, keyWidth+10, 18, 4)
		gtk.CairoSetSourceRGBA(cr, 248/255.0, 250/255.0, 252/255.0, 1)
		roundRect(cr, center-keyWidth/2-4, middle-14, keyWidth+8, 16, 3)
		gtk.CairoSetSourceRGBA(cr, 82/255.0, 97/255.0, 119/255.0, 1)
		pillText(cr, true, cancel, center-keyWidth/2, middle-3, 10, "monospace", false)
		captionWidth := pillText(cr, false, "to cancel", 0, 0, 9, "sans-serif", false)
		pillText(cr, true, "to cancel", center-captionWidth/2, middle+14, 9, "sans-serif", false)
	}
}

func pillText(cr uintptr, paint bool, text string, x, y, size float64, font string, bold bool) float64 {
	weight := int32(gtk.FontWeightNormal)
	if bold {
		weight = gtk.FontWeightBold
	}
	gtk.CairoSelectFontFace(cr, font, gtk.FontSlantNormal, weight)
	gtk.CairoSetFontSize(cr, size)
	var ext gtk.TextExtents
	gtk.CairoTextExtents(cr, text, &ext)
	if paint {
		gtk.CairoMoveTo(cr, x, y)
		gtk.CairoShowText(cr, text)
	}
	return ext.XAdvance
}

// roundRect fills a rectangle with round corners of radius r. The move first: show_text leaves a current point, and
// the first arc would draw a line from it.
func roundRect(cr uintptr, x, y, w, h, r float64) {
	gtk.CairoMoveTo(cr, x+w-r, y)
	gtk.CairoArc(cr, x+w-r, y+r, r, -math.Pi/2, 0)
	gtk.CairoArc(cr, x+w-r, y+h-r, r, 0, math.Pi/2)
	gtk.CairoArc(cr, x+r, y+h-r, r, math.Pi/2, math.Pi)
	gtk.CairoArc(cr, x+r, y+r, r, math.Pi, 3*math.Pi/2)
	gtk.CairoFill(cr)
}

func circle(cr uintptr, x, y, r float64) {
	gtk.CairoMoveTo(cr, x+r, y)
	gtk.CairoArc(cr, x, y, r, 0, 2*math.Pi)
	gtk.CairoFill(cr)
}

// notice is the pill on Wayland without layer-shell (GNOME): a critical notification while recording, closed when it
// ends. No timer: updating it would pop the banner up again every second. D-Bus runs off the GTK thread; ids hands
// each notification's id from its show to its hide, in order. ponytail: quitting the tray mid-recording leaves it up
// (the recording goes on, detached); close it at exit if that bites.
type notice struct{ ids chan uint32 }

func (n *notice) show(phase string, hint []Segment) {
	var body strings.Builder
	for _, s := range hint {
		body.WriteString(s.Text)
	}
	previous := n.ids
	n.ids = make(chan uint32, 1)
	title := map[string]string{"recording": "Listening", "busy": "Transcribing", "transform": "Refining text",
		"copy_failed": "Copy failed"}[phase]
	go func(ids chan<- uint32) {
		if previous != nil {
			desktop.CloseNotification(<-previous)
		}
		ids <- desktop.Critical(title, body.String())
	}(n.ids)
}

func (n *notice) hide() {
	if n.ids != nil {
		go func(ids <-chan uint32) { desktop.CloseNotification(<-ids) }(n.ids)
		n.ids = nil
	}
}
