// Package gtk binds the slice of GTK 3, GDK, GLib, cairo, Ayatana AppIndicator and gtk-layer-shell that `habla tray`
// needs, through purego (dlopen, no cgo). Nothing is loaded until Load, so `listen` and `say` never pay for GTK.
//
// Every function must run on the GTK thread (the main OS thread, which runs Main), except Idle, which any goroutine
// may call to hand work to it. Objects are plain uintptrs (GtkWidget*, cairo_t*, ...).
package gtk

import (
	"fmt"
	"sync"

	"github.com/ebitengine/purego"
)

// Constants from the C headers.
const (
	WindowToplevel = 0 // GtkWindowType
	WindowPopup    = 1

	CategoryApplicationStatus = 0 // AppIndicatorCategory
	StatusPassive             = 0 // AppIndicatorStatus
	StatusActive              = 1
	StatusAttention           = 2

	LayerOverlay     = 3 // GtkLayerShellLayer
	EdgeBottom       = 3 // GtkLayerShellEdge
	KeyboardModeNone = 0 // GtkLayerShellKeyboardMode

	FontSlantNormal  = 0 // cairo_font_slant_t
	FontWeightNormal = 0 // cairo_font_weight_t
	FontWeightBold   = 1
	OperatorSource   = 1 // cairo_operator_t
	OperatorOver     = 2
	FormatARGB32     = 0 // cairo_format_t
)

// Rect is a GdkRectangle.
type Rect struct{ X, Y, Width, Height int32 }

// TextExtents is a cairo_text_extents_t.
type TextExtents struct{ XBearing, YBearing, Width, Height, XAdvance, YAdvance float64 }

// GTK, GDK and GLib.
var (
	initCheck                           func(argc, argv uintptr) bool
	Main                                func()
	MainQuit                            func()
	WindowNew                           func(kind int32) uintptr
	WindowMove                          func(window uintptr, x, y int32)
	WindowResize                        func(window uintptr, width, height int32)
	WindowSetAcceptFocus                func(window uintptr, accept bool)
	DrawingAreaNew                      func() uintptr
	ContainerAdd                        func(container, widget uintptr)
	WidgetShow                          func(widget uintptr)
	WidgetShowAll                       func(widget uintptr)
	WidgetHide                          func(widget uintptr)
	WidgetDestroy                       func(widget uintptr)
	WidgetSetSensitive                  func(widget uintptr, sensitive bool)
	WidgetQueueDraw                     func(widget uintptr)
	WidgetSetSizeRequest                func(widget uintptr, width, height int32)
	WidgetGetAllocatedWidth             func(widget uintptr) int32
	WidgetGetAllocatedHeight            func(widget uintptr) int32
	WidgetSetVisual                     func(widget, visual uintptr)
	WidgetSetAppPaintable               func(widget uintptr, paintable bool)
	WidgetInputShapeCombineRegion       func(widget, region uintptr)
	MenuNew                             func() uintptr
	MenuShellAppend                     func(menu, item uintptr)
	MenuItemNewWithLabel                func(label string) uintptr
	MenuItemSetLabel                    func(item uintptr, label string)
	MenuItemSetSubmenu                  func(item, submenu uintptr)
	MenuItemActivate                    func(item uintptr)
	RadioMenuItemNewWithLabelFromWidget func(group uintptr, label string) uintptr
	CheckMenuItemNewWithLabel           func(label string) uintptr
	CheckMenuItemGetActive              func(item uintptr) bool
	CheckMenuItemSetActive              func(item uintptr, active bool)
	SeparatorMenuItemNew                func() uintptr
	ScreenGetDefault                    func() uintptr
	ScreenGetRGBAVisual                 func(screen uintptr) uintptr
	ScreenIsComposited                  func(screen uintptr) bool
	DisplayGetDefault                   func() uintptr
	DisplayGetNMonitors                 func(display uintptr) int32
	DisplayGetMonitor                   func(display uintptr, n int32) uintptr
	DisplayGetMonitorAtPoint            func(display uintptr, x, y int32) uintptr
	MonitorGetGeometry                  func(monitor uintptr, out *Rect)
	MonitorGetWorkarea                  func(monitor uintptr, out *Rect)
	MonitorGetScaleFactor               func(monitor uintptr) int32
	SourceRemove                        func(id uint32) bool
	idleAddFull                         func(priority int32, fn, data, notify uintptr) uint32
	timeoutAddFull                      func(priority int32, ms uint32, fn, data, notify uintptr) uint32
	signalConnectData                   func(obj uintptr, signal string, fn, data, notify uintptr, flags int32) uint64
)

// Cairo.
var (
	CairoSetSourceRGBA  func(cr uintptr, r, g, b, a float64)
	CairoSetOperator    func(cr uintptr, op int32)
	CairoPaint          func(cr uintptr)
	CairoArc            func(cr uintptr, xc, yc, radius, angle1, angle2 float64)
	CairoRectangle      func(cr uintptr, x, y, width, height float64)
	CairoFill           func(cr uintptr)
	CairoMoveTo         func(cr uintptr, x, y float64)
	CairoLineTo         func(cr uintptr, x, y float64)
	CairoSelectFontFace func(cr uintptr, family string, slant, weight int32)
	CairoSetFontSize    func(cr uintptr, size float64)
	CairoShowText       func(cr uintptr, text string)
	CairoTextExtents    func(cr uintptr, text string, out *TextExtents)
	CairoRegionCreate   func() uintptr // empty: the click-through input shape
	CairoRegionDestroy  func(region uintptr)

	CairoImageSurfaceCreate func(format, width, height int32) uintptr // a scratch surface: text widths before drawing
	CairoCreate             func(surface uintptr) uintptr
)

// Ayatana AppIndicator.
var (
	IndicatorNew                        func(id, icon string, category int32) uintptr
	IndicatorSetStatus                  func(indicator uintptr, status int32)
	IndicatorSetIconFull                func(indicator uintptr, icon, description string)
	IndicatorSetLabel                   func(indicator uintptr, label, guide string)
	IndicatorSetMenu                    func(indicator, menu uintptr)
	IndicatorSetSecondaryActivateTarget func(indicator, widget uintptr)
	IndicatorSetIconThemePath           func(indicator uintptr, path string)
)

// gtk-layer-shell: bound only when HasLayerShell.
var (
	LayerIsSupported     func() bool
	LayerInitForWindow   func(window uintptr)
	LayerSetNamespace    func(window uintptr, namespace string)
	LayerSetLayer        func(window uintptr, layer int32)
	LayerSetAnchor       func(window uintptr, edge int32, anchor bool)
	LayerSetMargin       func(window uintptr, edge, margin int32)
	LayerSetKeyboardMode func(window uintptr, mode int32)
	LayerSetMonitor      func(window, monitor uintptr) // 0: the compositor picks the output
)

type lib struct {
	name, pkg string
	optional  bool
	funcs     map[string]any // C symbol → pointer to the Go func var
}

var libs = []lib{
	{"libglib-2.0.so.0", "libglib2.0-0", false, map[string]any{
		"g_idle_add_full": &idleAddFull, "g_timeout_add_full": &timeoutAddFull, "g_source_remove": &SourceRemove,
	}},
	{"libgobject-2.0.so.0", "libglib2.0-0", false, map[string]any{"g_signal_connect_data": &signalConnectData}},
	{"libgdk-3.so.0", "libgtk-3-0", false, map[string]any{
		"gdk_screen_get_default": &ScreenGetDefault, "gdk_screen_get_rgba_visual": &ScreenGetRGBAVisual,
		"gdk_screen_is_composited": &ScreenIsComposited, "gdk_display_get_default": &DisplayGetDefault,
		"gdk_display_get_n_monitors": &DisplayGetNMonitors, "gdk_display_get_monitor": &DisplayGetMonitor,
		"gdk_display_get_monitor_at_point": &DisplayGetMonitorAtPoint,
		"gdk_monitor_get_geometry":         &MonitorGetGeometry, "gdk_monitor_get_workarea": &MonitorGetWorkarea,
		"gdk_monitor_get_scale_factor": &MonitorGetScaleFactor,
	}},
	{"libgtk-3.so.0", "libgtk-3-0", false, map[string]any{
		"gtk_init_check": &initCheck, "gtk_main": &Main, "gtk_main_quit": &MainQuit,
		"gtk_window_new": &WindowNew, "gtk_window_move": &WindowMove, "gtk_window_set_accept_focus": &WindowSetAcceptFocus,
		"gtk_window_resize": &WindowResize, "gtk_drawing_area_new": &DrawingAreaNew, "gtk_container_add": &ContainerAdd,
		"gtk_widget_show": &WidgetShow, "gtk_widget_show_all": &WidgetShowAll, "gtk_widget_hide": &WidgetHide,
		"gtk_widget_destroy": &WidgetDestroy, "gtk_widget_set_sensitive": &WidgetSetSensitive,
		"gtk_widget_queue_draw": &WidgetQueueDraw, "gtk_widget_set_size_request": &WidgetSetSizeRequest,
		"gtk_widget_get_allocated_width": &WidgetGetAllocatedWidth, "gtk_widget_get_allocated_height": &WidgetGetAllocatedHeight,
		"gtk_widget_set_visual": &WidgetSetVisual, "gtk_widget_set_app_paintable": &WidgetSetAppPaintable,
		"gtk_widget_input_shape_combine_region": &WidgetInputShapeCombineRegion,
		"gtk_menu_new":                          &MenuNew, "gtk_menu_shell_append": &MenuShellAppend,
		"gtk_menu_item_new_with_label": &MenuItemNewWithLabel, "gtk_menu_item_set_label": &MenuItemSetLabel,
		"gtk_menu_item_set_submenu": &MenuItemSetSubmenu, "gtk_menu_item_activate": &MenuItemActivate,
		"gtk_radio_menu_item_new_with_label_from_widget": &RadioMenuItemNewWithLabelFromWidget,
		"gtk_check_menu_item_new_with_label":             &CheckMenuItemNewWithLabel,
		"gtk_check_menu_item_get_active":                 &CheckMenuItemGetActive,
		"gtk_check_menu_item_set_active":                 &CheckMenuItemSetActive,
		"gtk_separator_menu_item_new":                    &SeparatorMenuItemNew,
	}},
	{"libcairo.so.2", "libcairo2", false, map[string]any{
		"cairo_image_surface_create": &CairoImageSurfaceCreate, "cairo_create": &CairoCreate,
		"cairo_set_source_rgba": &CairoSetSourceRGBA, "cairo_set_operator": &CairoSetOperator, "cairo_paint": &CairoPaint,
		"cairo_arc": &CairoArc, "cairo_rectangle": &CairoRectangle, "cairo_fill": &CairoFill, "cairo_move_to": &CairoMoveTo, "cairo_line_to": &CairoLineTo,
		"cairo_select_font_face": &CairoSelectFontFace, "cairo_set_font_size": &CairoSetFontSize,
		"cairo_show_text": &CairoShowText, "cairo_text_extents": &CairoTextExtents,
		"cairo_region_create": &CairoRegionCreate, "cairo_region_destroy": &CairoRegionDestroy,
	}},
	{"libayatana-appindicator3.so.1", "libayatana-appindicator3-1", false, map[string]any{
		"app_indicator_new": &IndicatorNew, "app_indicator_set_status": &IndicatorSetStatus,
		"app_indicator_set_icon_full": &IndicatorSetIconFull, "app_indicator_set_label": &IndicatorSetLabel,
		"app_indicator_set_menu":                      &IndicatorSetMenu,
		"app_indicator_set_secondary_activate_target": &IndicatorSetSecondaryActivateTarget,
		"app_indicator_set_icon_theme_path":           &IndicatorSetIconThemePath,
	}},
	{"libgtk-layer-shell.so.0", "libgtk-layer-shell0", true, map[string]any{
		"gtk_layer_is_supported": &LayerIsSupported, "gtk_layer_init_for_window": &LayerInitForWindow,
		"gtk_layer_set_namespace": &LayerSetNamespace, "gtk_layer_set_layer": &LayerSetLayer,
		"gtk_layer_set_anchor": &LayerSetAnchor, "gtk_layer_set_margin": &LayerSetMargin,
		"gtk_layer_set_keyboard_mode": &LayerSetKeyboardMode, "gtk_layer_set_monitor": &LayerSetMonitor,
	}},
}

var (
	loadOnce      sync.Once
	loadErr       error
	hasLayerShell bool
	// The only C callbacks: purego can make about 2000 per process and never frees them, so every signal, idle and
	// timeout goes through one of these, with a handles key as user_data.
	signalCB, drawCB, sourceCB, destroyCB uintptr
)

// Load opens and binds the libraries, once. A missing required library (or symbol) is an error naming the Debian
// package; a missing or incomplete gtk-layer-shell only makes HasLayerShell false.
func Load() error {
	loadOnce.Do(func() {
		for _, l := range libs {
			err := bind(l)
			if l.optional {
				hasLayerShell = err == nil
			} else if err != nil {
				loadErr = err
				return
			}
		}
		signalCB = purego.NewCallback(func(_, data uintptr) { handle(data).(func())() })
		drawCB = purego.NewCallback(func(_, cr, data uintptr) uintptr { handle(data).(func(uintptr))(cr); return 0 })
		sourceCB = purego.NewCallback(func(data uintptr) uintptr {
			if handle(data).(func() bool)() {
				return 1 // G_SOURCE_CONTINUE
			}
			return 0
		})
		// GClosureNotify (data, closure) and GDestroyNotify (data) alike: the handler or source is gone.
		destroyCB = purego.NewCallback(func(data, _ uintptr) { drop(data) })
	})
	return loadErr
}

func bind(l lib) error {
	h, err := purego.Dlopen(l.name, purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		return fmt.Errorf("cannot load %s (install %s): %w", l.name, l.pkg, err)
	}
	for name, fn := range l.funcs {
		sym, err := purego.Dlsym(h, name)
		if err != nil {
			return fmt.Errorf("%s has no %s (install a newer %s)", l.name, name, l.pkg)
		}
		purego.RegisterFunc(fn, sym)
	}
	return nil
}

// HasLayerShell reports whether Load bound gtk-layer-shell.
func HasLayerShell() bool { return hasLayerShell }

// Init connects GTK to the display ($DISPLAY or $WAYLAND_DISPLAY).
func Init() error {
	if !initCheck(0, 0) {
		return fmt.Errorf("cannot open the display")
	}
	return nil
}

// Connect calls f on each emission of a signal whose handler takes only the instance, such as "activate" and
// "toggled". The handler is freed with the object.
func Connect(obj uintptr, signal string, f func()) {
	signalConnectData(obj, signal, signalCB, keep(f), destroyCB, 0)
}

// ConnectDraw calls f with the cairo context on each "draw" of a widget; the default drawing still runs after it.
func ConnectDraw(widget uintptr, f func(cr uintptr)) {
	signalConnectData(widget, "draw", drawCB, keep(f), destroyCB, 0)
}

// Idle runs f once on the GTK thread. Safe from any goroutine.
func Idle(f func()) {
	idleAddFull(200, sourceCB, keep(func() bool { f(); return false }), destroyCB) // G_PRIORITY_DEFAULT_IDLE
}

// Timeout calls f on the GTK thread every ms milliseconds while it returns true. SourceRemove(id) stops it.
func Timeout(ms uint32, f func() bool) (id uint32) {
	return timeoutAddFull(0, ms, sourceCB, keep(f), destroyCB) // G_PRIORITY_DEFAULT
}

var (
	mu      sync.Mutex
	last    uintptr
	handles = map[uintptr]any{}
)

func keep(f any) uintptr {
	mu.Lock()
	defer mu.Unlock()
	last++
	handles[last] = f
	return last
}

func handle(key uintptr) any {
	mu.Lock()
	defer mu.Unlock()
	return handles[key]
}

func drop(key uintptr) {
	mu.Lock()
	defer mu.Unlock()
	delete(handles, key)
}
