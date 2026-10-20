package desktop

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"
	"github.com/jezek/xgb/xtest"
)

// Terminals get Ctrl+Shift+V with paste_keys auto: these WM_CLASS names, or any containing "terminal".
// IDEs with a terminal panel are deliberately not matched; set paste_keys for those.
var terminals = []string{"terminator", "gnome-terminal", "mate-terminal", "xfce4-terminal", "konsole", "lxterminal",
	"qterminal", "tilix", "alacritty", "kitty", "foot", "wezterm", "st", "urxvt", "xterm", "deepin-terminal"}

const (
	keysymShiftL   xproto.Keysym = 0xffe1
	keysymControlL xproto.Keysym = 0xffe3
	keysymV        xproto.Keysym = 0x0076
	keysymReturn   xproto.Keysym = 0xff0d
)

// IsTerminal reports whether a window with these WM_CLASS names is a terminal emulator.
func IsTerminal(classNames []string) bool {
	for _, name := range classNames {
		name = strings.ToLower(name)
		if slices.Contains(terminals, name) || strings.Contains(name, "terminal") {
			return true
		}
	}
	return false
}

// ClassNames splits a WM_CLASS value ("instance\0class\0").
func ClassNames(wmClass []byte) []string {
	return strings.FieldsFunc(string(wmClass), func(r rune) bool { return r == 0 })
}

// pasteX11 sends the chord to the focused window with XTEST, after releasing the modifier keys still down (the
// hotkey's Super, say), like xdotool --clearmodifiers. ponytail: released modifiers are not pressed again.
func pasteX11(pasteKeys string, enter bool) error {
	conn, err := xgb.NewConn()
	if err != nil {
		return fmt.Errorf("no X display: %w", err)
	}
	defer conn.Close()
	if err := xtest.Init(conn); err != nil {
		return fmt.Errorf("XTEST is not available: %w", err)
	}
	setup := xproto.Setup(conn)
	root := setup.DefaultScreen(conn).Root
	keys := pasteKeys
	if keys == "auto" {
		keys = "ctrl+v"
		if IsTerminal(windowClass(conn, root)) {
			keys = "ctrl+shift+v"
		}
	}
	codes, err := Keycodes(conn, setup)
	if err != nil {
		return err
	}
	chord := []xproto.Keycode{codes[keysymControlL]}
	if keys == "ctrl+shift+v" {
		chord = append(chord, codes[keysymShiftL])
	}
	chord = append(chord, codes[keysymV])
	if slices.Contains(chord, 0) {
		return errors.New("the keyboard map has no key for " + keys)
	}
	if enter && codes[keysymReturn] == 0 {
		return errors.New("the keyboard map has no key for Enter")
	}
	releaseModifiers(conn, root)
	var first error // ponytail: keep sending the releases after a failure so no key stays down
	send := func(kind byte, code xproto.Keycode) {
		if err := xtest.FakeInputChecked(conn, kind, byte(code), 0, root, 0, 0, 0).Check(); err != nil && first == nil {
			first = fmt.Errorf("XTEST keystroke failed: %w", err)
		}
	}
	for _, code := range chord {
		send(xproto.KeyPress, code)
	}
	for i := len(chord) - 1; i >= 0; i-- {
		send(xproto.KeyRelease, chord[i])
	}
	if first != nil {
		return first
	}
	if enter {
		time.Sleep(enterDelay)
		send(xproto.KeyPress, codes[keysymReturn])
		send(xproto.KeyRelease, codes[keysymReturn])
		if first != nil {
			return fmt.Errorf("pasted, but could not press Enter: %w", first)
		}
	}
	_, err = xproto.GetInputFocus(conn).Reply() // a round trip: the server has handled the events before we close
	return err
}

// ActiveWindow is the window in _NET_ACTIVE_WINDOW; 0 when unknown.
func ActiveWindow(conn *xgb.Conn, root xproto.Window) xproto.Window {
	name := "_NET_ACTIVE_WINDOW"
	atom, err := xproto.InternAtom(conn, true, uint16(len(name)), name).Reply()
	if err != nil || atom.Atom == 0 {
		return 0
	}
	active, err := xproto.GetProperty(conn, false, root, atom.Atom, xproto.AtomWindow, 0, 1).Reply()
	if err != nil || len(active.Value) < 4 {
		return 0
	}
	return xproto.Window(xgb.Get32(active.Value))
}

// windowClass is the WM_CLASS of the active window; nil when unknown.
func windowClass(conn *xgb.Conn, root xproto.Window) []string {
	window := ActiveWindow(conn, root)
	if window == 0 {
		return nil
	}
	class, err := xproto.GetProperty(conn, false, window, xproto.AtomWmClass, xproto.AtomString, 0, 64).Reply()
	if err != nil {
		return nil
	}
	return ClassNames(class.Value)
}

// Keycodes maps each keysym of the keyboard map to the first keycode that produces it.
func Keycodes(conn *xgb.Conn, setup *xproto.SetupInfo) (map[xproto.Keysym]xproto.Keycode, error) {
	count := byte(setup.MaxKeycode - setup.MinKeycode + 1)
	reply, err := xproto.GetKeyboardMapping(conn, setup.MinKeycode, count).Reply()
	if err != nil {
		return nil, err
	}
	per := int(reply.KeysymsPerKeycode)
	codes := map[xproto.Keysym]xproto.Keycode{}
	for i := 0; i < int(count); i++ {
		for _, sym := range reply.Keysyms[i*per : (i+1)*per] {
			if _, seen := codes[sym]; !seen {
				codes[sym] = setup.MinKeycode + xproto.Keycode(i)
			}
		}
	}
	return codes, nil
}

// releaseModifiers sends a release for every modifier key that is down, except Lock and Mod2 (CapsLock, NumLock),
// which are toggles.
func releaseModifiers(conn *xgb.Conn, root xproto.Window) {
	down, err := xproto.QueryKeymap(conn).Reply()
	if err != nil {
		return
	}
	mapping, err := xproto.GetModifierMapping(conn).Reply()
	if err != nil {
		return
	}
	per := int(mapping.KeycodesPerModifier)
	for mod := 0; mod < 8; mod++ {
		if mod == 1 || mod == 4 { // Lock, Mod2
			continue
		}
		for _, code := range mapping.Keycodes[mod*per : (mod+1)*per] {
			if b := byte(code); b != 0 && down.Keys[b/8]&(1<<(b%8)) != 0 {
				xtest.FakeInputChecked(conn, xproto.KeyRelease, byte(code), 0, root, 0, 0, 0).Check() // best effort: errors ignored
			}
		}
	}
}
