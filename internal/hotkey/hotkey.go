// Package hotkey is the hold-to-talk logic, with no X import so it is tested without a display.
// The tray's xgb loop turns X key events into Events.
package hotkey

import (
	"fmt"
	"strings"
)

// Event is one key press or release. Keycode is the X keycode (PortalKey for the portal's shortcut); State is the X
// modifier mask at the event; Time is the X server timestamp.
type Event struct {
	Press   bool
	Keycode int
	State   uint16
	Time    uint32
}

// X11 modifier masks.
const (
	Shift   uint16 = 1
	Lock    uint16 = 2
	Control uint16 = 4
	Mod1    uint16 = 8
	Mod2    uint16 = 16
	Mod4    uint16 = 64
)

const (
	PortalKey = 0  // the portal's shortcut as a Hold keycode (no X keycode is 0)
	NoEscape  = -1 // the portal has no Esc
)

var modifiers = map[string]uint16{"shift": Shift, "control": Control, "ctrl": Control, "primary": Control, "alt": Mod1, "super": Mod4}

// portalModifiers are the XDG shortcuts spec names, in the order PortalTrigger writes them.
var portalModifiers = []struct {
	bit  uint16
	name string
}{{Control, "CTRL"}, {Mod1, "ALT"}, {Shift, "SHIFT"}, {Mod4, "LOGO"}}

var keysyms = map[string]uint32{"space": 0x20, "Return": 0xff0d, "Escape": 0xff1b, "Tab": 0xff09}

func init() {
	for c := 'a'; c <= 'z'; c++ { // the keysyms of Latin-1 letters and digits are their ASCII codes
		keysyms[string(c)] = uint32(c)
		keysyms[strings.ToUpper(string(c))] = uint32(c - 32)
	}
	for c := '0'; c <= '9'; c++ {
		keysyms[string(c)] = uint32(c)
	}
	for n := 1; n <= 12; n++ {
		keysyms[fmt.Sprintf("F%d", n)] = 0xffbd + uint32(n)
	}
}

// Keysym returns the X keysym of a key name Parse can return; ok is false for names outside the table.
func Keysym(name string) (keysym uint32, ok bool) {
	keysym, ok = keysyms[name]
	return
}

// Parse turns `<Super>space` (GTK accelerator style) into an X11 modifier mask and a keysym name.
func Parse(text string) (mask uint16, key string, err error) {
	mods, key := acceleratorParts(text)
	for _, m := range mods {
		bit, ok := modifiers[strings.ToLower(m)]
		if !ok {
			return 0, "", fmt.Errorf("unknown modifier <%s>", m)
		}
		mask |= bit
	}
	if key == "" {
		return 0, "", fmt.Errorf("no key after the modifiers")
	}
	return mask, key, nil
}

func acceleratorParts(text string) (mods []string, key string) {
	for {
		before, rest, found := strings.Cut(text, "<")
		if !found {
			break
		}
		mod, after, closed := strings.Cut(rest, ">")
		if !closed {
			break
		}
		mods = append(mods, mod)
		key += before
		text = after
	}
	return mods, strings.TrimSpace(key + text)
}

func capitalize(s string) string { return strings.ToUpper(s[:1]) + s[1:] }

// Label turns `<Super>space` into `Super+Space`, for the menu.
func Label(text string) (string, error) {
	_, key, err := Parse(text)
	if err != nil {
		return "", err
	}
	var parts []string
	mods, _ := acceleratorParts(text)
	for _, m := range mods {
		parts = append(parts, capitalize(strings.ToLower(m)))
	}
	return strings.Join(append(parts, capitalize(key)), "+"), nil
}

// PortalTrigger turns `<Super>space` into `LOGO+space`: the GlobalShortcuts portal's preferred_trigger.
func PortalTrigger(text string) (string, error) {
	mask, key, err := Parse(text)
	if err != nil {
		return "", err
	}
	var parts []string
	for _, m := range portalModifiers {
		if mask&m.bit != 0 {
			parts = append(parts, m.name)
		}
	}
	return strings.Join(append(parts, key), "+"), nil
}

// PortalEvent turns a portal Activated / Deactivated signal into the press / release that Hold understands.
func PortalEvent(signal string, timestamp uint32) Event {
	return Event{Press: signal == "Activated", Keycode: PortalKey, Time: timestamp}
}

// GrabMasks lists the four lock variants to grab: the hotkey stops working while CapsLock or NumLock is on otherwise.
// ponytail: NumLock is Mod2 on mainstream X setups; look Num_Lock up in the modifier mapping if one differs.
func GrabMasks(mask uint16) []uint16 {
	return []uint16{mask, mask | Lock, mask | Mod2, mask | Lock | Mod2}
}

// DropAutoRepeat drops X11 auto-repeat: a release immediately followed by a press of the same key, same timestamp.
// ponytail: no XKB detectable auto-repeat in xgb. A pair split across two reads is not seen (stop + new start); the
// server writes both events together, so it doesn't happen in practice.
func DropAutoRepeat(events []Event) []Event {
	var kept []Event
	for i := 0; i < len(events); i++ {
		if e := events[i]; i+1 < len(events) && !e.Press && events[i+1].Press &&
			e.Keycode == events[i+1].Keycode && e.Time == events[i+1].Time {
			i++
			continue
		}
		kept = append(kept, events[i])
	}
	return kept
}

// Hold turns hotkey events into "start" (press), "stop" (release), "cancel" (Esc while held, or while busy) or "send"
// (Enter while busy).
type Hold struct {
	keycode, escape, enter int
	mask                   uint16 // the hotkey's modifiers: Enter and a <Super>Return hotkey share a keycode
	held, cancelled        bool
}

func NewHold(keycode, escape int) *Hold {
	return &Hold{keycode: keycode, escape: escape, enter: NoEscape}
}

// WithEnter makes a press of enter while not held a "send", unless it carries the hotkey's modifiers (mask).
func (h *Hold) WithEnter(enter int, mask uint16) *Hold {
	h.enter, h.mask = enter, mask
	return h
}

// Active is true from the press to the release, Esc included: the keyboard stays grabbed meanwhile.
func (h *Hold) Active() bool { return h.held || h.cancelled }

func (h *Hold) Feed(events []Event) (actions []string) {
	for _, e := range DropAutoRepeat(events) {
		switch {
		case e.Press && e.Keycode == h.keycode && e.State&h.mask == h.mask && !h.Active():
			h.held = true
			actions = append(actions, "start")
		case e.Press && e.Keycode == h.escape && h.held:
			h.held, h.cancelled = false, true
			actions = append(actions, "cancel")
		case e.Press && e.Keycode == h.escape && !h.Active(): // Esc reaches Habla unheld only while busy
			actions = append(actions, "cancel")
		case e.Press && e.Keycode == h.enter && !h.Active(): // so does Enter
			actions = append(actions, "send")
		case !e.Press && e.Keycode == h.keycode && h.Active():
			if h.held {
				actions = append(actions, "stop")
			}
			h.held, h.cancelled = false, false
		}
	}
	return actions
}
