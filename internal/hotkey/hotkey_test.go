package hotkey

import (
	"reflect"
	"strings"
	"testing"
)

const space, esc, super = 65, 9, 133

func press(code int, time uint32) Event   { return Event{Press: true, Keycode: code, Time: time} }
func release(code int, time uint32) Event { return Event{Keycode: code, Time: time} }

func TestParse(t *testing.T) {
	for _, c := range []struct {
		text string
		mask uint16
		key  string
	}{
		{"<Super>space", 64, "space"},
		{"<Control><Alt>F12", 4 | 8, "F12"},
		{"<ctrl><shift>a", 4 | 1, "a"},
		{"<Primary>space", 4, "space"},
		{"F9", 0, "F9"},
		{" sp<Super>ace ", 64, "space"},
		{"<Super><Super>space", 64, "space"},
		{"space<unfinished", 0, "space<unfinished"},
		{"space>", 0, "space>"},
	} {
		mask, key, err := Parse(c.text)
		if err != nil || mask != c.mask || key != c.key {
			t.Errorf("Parse(%q) = %d, %q, %v", c.text, mask, key, err)
		}
	}
}

func TestParseRejects(t *testing.T) {
	for text, msg := range map[string]string{"<Hyper>space": "unknown modifier <Hyper>", "<Super>": "no key", "<>space": "unknown modifier <>", "<<Super>space": "unknown modifier <<Super>", "  ": "no key"} {
		if _, _, err := Parse(text); err == nil || !strings.Contains(err.Error(), msg) {
			t.Errorf("Parse(%q) error = %v, want %q", text, err, msg)
		}
	}
}

func TestLabel(t *testing.T) {
	for text, shown := range map[string]string{"<Super>space": "Super+Space", "<Control><Alt>F12": "Control+Alt+F12", "F9": "F9", "sp<sUPER>ace": "Super+Space", "<Super><Super>space": "Super+Super+Space"} {
		if got, _ := Label(text); got != shown {
			t.Errorf("Label(%q) = %q, want %q", text, got, shown)
		}
	}
	if _, err := Label("<Hyper>x"); err == nil {
		t.Error("Label accepted an unknown modifier")
	}
}

func TestKeysym(t *testing.T) {
	for name, want := range map[string]uint32{"space": 0x20, "a": 0x61, "A": 0x41, "7": 0x37, "F1": 0xffbe, "F12": 0xffc9,
		"Return": 0xff0d, "Escape": 0xff1b, "Tab": 0xff09} {
		if got, ok := Keysym(name); !ok || got != want {
			t.Errorf("Keysym(%q) = %#x, %v, want %#x", name, got, ok, want)
		}
	}
	for _, name := range []string{"F13", "nonsense", ""} {
		if _, ok := Keysym(name); ok {
			t.Errorf("Keysym(%q) should be unknown", name)
		}
	}
}

func TestGrabMasksCoverCapsLockAndNumLock(t *testing.T) {
	want := []uint16{64, 64 | 2, 64 | 16, 64 | 2 | 16}
	if got := GrabMasks(64); !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestDropAutoRepeatRemovesSameKeySameTimePairs(t *testing.T) {
	events := []Event{press(space, 1), release(space, 500), press(space, 500), release(space, 533), press(space, 533)}
	if got, want := DropAutoRepeat(events), []Event{press(space, 1)}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestDropAutoRepeatKeepsRealEvents(t *testing.T) {
	for name, events := range map[string][]Event{
		"different time":       {release(space, 5), press(space, 6)},
		"different key":        {release(space, 5), press(esc, 5)},
		"press then release":   {press(space, 5), release(space, 5)},
		"release at batch end": {release(space, 5)},
	} {
		if got := DropAutoRepeat(events); !reflect.DeepEqual(got, events) {
			t.Errorf("%s: got %v", name, got)
		}
	}
}

// feed checks one batch of events against the expected actions (nil = none).
func feed(t *testing.T, h *Hold, want []string, events ...Event) {
	t.Helper()
	if got := h.Feed(events); !reflect.DeepEqual(got, want) {
		t.Fatalf("Feed(%v) = %v, want %v", events, got, want)
	}
}

func active(t *testing.T, h *Hold, want bool) {
	t.Helper()
	if h.Active() != want {
		t.Fatalf("Active() = %v, want %v", h.Active(), want)
	}
}

func TestPressStartsAndReleaseStops(t *testing.T) {
	h := NewHold(space, esc)
	feed(t, h, []string{"start"}, press(space, 1))
	active(t, h, true)
	feed(t, h, []string{"stop"}, release(space, 900))
	active(t, h, false)
}

func TestAutoRepeatWhileHeldDoesNothing(t *testing.T) {
	h := NewHold(space, esc)
	feed(t, h, []string{"start"}, press(space, 1))
	feed(t, h, nil, release(space, 500), press(space, 500), release(space, 533), press(space, 533))
	active(t, h, true)
	feed(t, h, []string{"stop"}, release(space, 700))
}

func TestRepeatedPressWithoutReleaseDoesNothing(t *testing.T) {
	feed(t, NewHold(space, esc), []string{"start"}, press(space, 1), press(space, 500), press(space, 533))
}

func TestEscCancelsAndTheReleaseThenDoesNothing(t *testing.T) {
	h := NewHold(space, esc)
	feed(t, h, []string{"start"}, press(space, 1))
	feed(t, h, []string{"cancel"}, press(esc, 3), release(esc, 4))
	active(t, h, true) // the keyboard stays grabbed until the hotkey is released
	feed(t, h, nil, press(space, 5))
	feed(t, h, nil, release(space, 6))
	active(t, h, false)
	feed(t, h, []string{"start"}, press(space, 7))
}

func TestEscWhenNotHeldCancels(t *testing.T) { // the tray grabs Esc only while busy
	h := NewHold(space, esc)
	feed(t, h, []string{"cancel"}, press(esc, 1), release(esc, 2))
	active(t, h, false)
}

func TestEnterWhenNotHeldSendsEvenWhenItIsTheHotkeysKey(t *testing.T) { // the tray grabs Enter only while busy
	const enter, kpEnter = 36, 104
	h := NewHold(enter, esc).WithEnter(Mod4, enter, kpEnter) // <Super>Return
	feed(t, h, []string{"send"}, press(enter, 1), release(enter, 2))
	feed(t, h, []string{"send"}, press(kpEnter, 1), release(kpEnter, 2))
	withSuper := Event{Press: true, Keycode: enter, State: Mod4 | Mod2, Time: 3} // NumLock on
	feed(t, h, []string{"start"}, withSuper)
	feed(t, h, []string{"stop"}, release(enter, 4)) // Super released first
}

func TestOtherKeysWhileHeldAreIgnored(t *testing.T) {
	h := NewHold(space, esc)
	feed(t, h, []string{"start"}, press(space, 1))
	feed(t, h, nil, release(super, 2), press(super, 3))
	active(t, h, true)
	feed(t, h, []string{"stop"}, release(space, 4)) // Super released first: the space release still stops
}

func TestReleaseWithoutPressDoesNothing(t *testing.T) {
	feed(t, NewHold(space, esc), nil, release(space, 1))
}

func TestPortalTrigger(t *testing.T) {
	for text, trigger := range map[string]string{
		"<Super>space": "LOGO+space", "<Control><Alt>F12": "CTRL+ALT+F12", "<Primary><Shift>a": "CTRL+SHIFT+a", "F9": "F9",
	} {
		if got, err := PortalTrigger(text); err != nil || got != trigger {
			t.Errorf("PortalTrigger(%q) = %q, %v, want %q", text, got, err, trigger)
		}
	}
	if _, err := PortalTrigger("<Hyper>space"); err == nil || !strings.Contains(err.Error(), "unknown modifier") {
		t.Errorf("error = %v", err)
	}
}

func TestPortalActivatedStartsAndDeactivatedStops(t *testing.T) {
	h := NewHold(PortalKey, PortalCancel)
	feed(t, h, []string{"start"}, PortalEvent("Activated", PortalKey, 10))
	feed(t, h, nil, PortalEvent("Activated", PortalKey, 20)) // a repeated Activated while held
	feed(t, h, []string{"stop"}, PortalEvent("Deactivated", PortalKey, 900))
	feed(t, h, nil, PortalEvent("Deactivated", PortalKey, 950))
	feed(t, h, []string{"start"}, PortalEvent("Activated", PortalKey, 1000))
}

func TestEnterWhileHeldStopsAndSends(t *testing.T) {
	const enter = 36
	h := NewHold(space, esc).WithEnter(0, enter)
	feed(t, h, []string{"start"}, press(space, 1))
	feed(t, h, []string{"stop", "send"}, press(enter, 2), release(enter, 3))
	active(t, h, true) // the keyboard stays grabbed until the hotkey is released
	feed(t, h, nil, release(space, 4))
	active(t, h, false)

	p := NewHold(PortalKey, PortalCancel).WithEnter(0, PortalSend) // <Super>Return while <Super>space is held
	feed(t, p, []string{"start"}, PortalEvent("Activated", PortalKey, 10))
	feed(t, p, []string{"stop", "send"}, PortalEvent("Activated", PortalSend, 20),
		PortalEvent("Deactivated", PortalSend, 30))
	feed(t, p, nil, PortalEvent("Deactivated", PortalKey, 40))
	feed(t, p, []string{"send"}, PortalEvent("Activated", PortalSend, 50)) // while busy
	feed(t, p, []string{"cancel"}, PortalEvent("Activated", PortalCancel, 60))
}

func TestHotkeysOwnEnterWhileHeldIsNotASend(t *testing.T) { // <Super>Return repeating while held
	const enter = 36
	h := NewHold(enter, esc).WithEnter(Mod4, enter)
	feed(t, h, []string{"start"}, Event{Press: true, Keycode: enter, State: Mod4, Time: 1})
	feed(t, h, nil, Event{Press: true, Keycode: enter, State: Mod4, Time: 2})
	feed(t, h, []string{"stop"}, release(enter, 3))
}
