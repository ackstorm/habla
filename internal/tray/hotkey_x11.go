package tray

import (
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/ackstorm/habla/internal/desktop"
	"github.com/ackstorm/habla/internal/hotkey"
	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"
)

const keysymEscape, keysymReturn, keysymKPEnter = 0xff1b, 0xff0d, 0xff8d

// autoRepeatGap is how long a release waits for the press that would make it auto-repeat. xgb hands events over one
// at a time from its reader goroutine, so the pair may not be queued together yet. It delays a real release (stop)
// by this much.
const autoRepeatGap = 20 * time.Millisecond

// maxFocusLock bounds the focus lock after Enter, in case the dictation never leaves busy or transform.
const maxFocusLock = 60 * time.Second

var errTaken = errors.New("taken by another app")

// invalidHotkey is a hotkey that can't be parsed or that the keyboard map lacks.
type invalidHotkey struct{ error }

// noDisplay is a failure to connect to the X server.
type noDisplay struct{ error }

// x11Hotkey is X11 hold-to-talk: a passive grab on the hotkey in four lock-key variants on a
// connection of its own, plus a keyboard grab from the press to the release, so Esc reaches Habla and auto-repeat
// releases don't end the grab.
type x11Hotkey struct {
	conn                   *xgb.Conn
	root                   xproto.Window
	escape, enter, kpEnter xproto.Keycode
	lockMu                 sync.Mutex
	lockTimer              *time.Timer // non-nil while Enter holds the keyboard and pointer (lockFocus)
}

// grabX11 grabs text (`<Super>space`) and calls act("--start" | "--stop" | "--cancel" | "--send") from its goroutine.
func grabX11(text string, act func(flag string)) (*x11Hotkey, error) {
	mask, name, err := hotkey.Parse(text)
	if err != nil {
		return nil, invalidHotkey{err}
	}
	conn, err := xgb.NewConn()
	if err != nil {
		return nil, noDisplay{err}
	}
	setup := xproto.Setup(conn)
	root := setup.DefaultScreen(conn).Root
	codes, err := desktop.Keycodes(conn, setup)
	if err != nil {
		conn.Close()
		return nil, noDisplay{err}
	}
	sym, ok := hotkey.Keysym(name)
	code := codes[xproto.Keysym(sym)]
	if !ok || code == 0 {
		conn.Close()
		return nil, invalidHotkey{fmt.Errorf("unknown key '%s'", name)}
	}
	for _, m := range hotkey.GrabMasks(mask) {
		err := xproto.GrabKeyChecked(conn, true, root, m, code, xproto.GrabModeAsync, xproto.GrabModeAsync).Check()
		if err != nil {
			conn.Close() // releases the variants already grabbed
			if _, ok := err.(xproto.AccessError); ok {
				return nil, errTaken
			}
			return nil, noDisplay{err}
		}
	}
	h := &x11Hotkey{conn: conn, root: root, escape: codes[keysymEscape], enter: codes[keysymReturn], kpEnter: codes[keysymKPEnter]}
	go h.loop(root, hotkey.NewHold(int(code), int(h.escape)).WithEnter(mask, int(h.enter), int(h.kpEnter)), act)
	return h, nil
}

// grabBusyKeys grabs Esc and both Enters (on) or lets them go: while a dictation is busy, Esc cancels it and Enter sends it
// (Hold.Feed). ponytail: unchecked: a key taken by another app's grab just stays theirs.
func (h *x11Hotkey) grabBusyKeys(on bool) {
	for _, key := range []xproto.Keycode{h.escape, h.enter, h.kpEnter} {
		if key == 0 {
			continue
		}
		for _, m := range hotkey.GrabMasks(0) {
			if on {
				xproto.GrabKey(h.conn, true, h.root, m, key, xproto.GrabModeAsync, xproto.GrabModeAsync)
			} else {
				xproto.UngrabKey(h.conn, key, h.root, m)
			}
		}
	}
}

// lockFocus grabs the keyboard and pointer after Enter, so neither Alt+Tab nor a click moves the focus away from
// the window the dictation will be pasted into; Esc still cancels. It lasts through copying and ends at pasting (the
// tray's refresh; XTEST keys would reach this grab, so desktop.Paste waits for it) or after maxFocusLock.
func (h *x11Hotkey) lockFocus() {
	h.lockMu.Lock()
	defer h.lockMu.Unlock()
	if h.lockTimer != nil {
		return
	}
	xproto.GrabKeyboard(h.conn, false, h.root, xproto.TimeCurrentTime, xproto.GrabModeAsync, xproto.GrabModeAsync).Reply()
	xproto.GrabPointer(h.conn, false, h.root, 0, xproto.GrabModeAsync, xproto.GrabModeAsync, 0, 0, xproto.TimeCurrentTime).Reply()
	h.lockTimer = time.AfterFunc(maxFocusLock, func() { h.unlockFocus(true) })
}

// unlockFocus ends the focus lock, if any; keyboard false keeps the keyboard grabbed for a hold that took it over.
func (h *x11Hotkey) unlockFocus(keyboard bool) {
	h.lockMu.Lock()
	defer h.lockMu.Unlock()
	if h.lockTimer == nil {
		return
	}
	h.lockTimer.Stop()
	h.lockTimer = nil
	xproto.UngrabPointer(h.conn, xproto.TimeCurrentTime)
	if keyboard {
		xproto.UngrabKeyboard(h.conn, xproto.TimeCurrentTime)
	}
	xproto.GetInputFocus(h.conn).Reply() // a round trip: the grabs are gone when this returns
}

// locked is true while the focus lock holds the keyboard and pointer.
func (h *x11Hotkey) locked() bool {
	h.lockMu.Lock()
	defer h.lockMu.Unlock()
	return h.lockTimer != nil
}

// close releases the grabs and ends the loop.
func (h *x11Hotkey) close() { h.conn.Close() }

func (h *x11Hotkey) loop(root xproto.Window, hold *hotkey.Hold, act func(string)) {
	events := make(chan hotkey.Event)
	go func() {
		defer close(events)
		for {
			ev, err := h.conn.WaitForEvent()
			if ev == nil && err == nil {
				return // the connection is closed
			}
			switch e := ev.(type) {
			case xproto.KeyPressEvent:
				events <- hotkey.Event{Press: true, Keycode: int(e.Detail), State: e.State, Time: uint32(e.Time)}
			case xproto.KeyReleaseEvent:
				events <- hotkey.Event{Press: false, Keycode: int(e.Detail), State: e.State, Time: uint32(e.Time)}
			}
		}
	}()
	grabbed := false
	for first := range events {
		batch := []hotkey.Event{first}
		for {
			e, ok := next(events, !batch[len(batch)-1].Press)
			if !ok {
				break
			}
			batch = append(batch, e)
		}
		for _, action := range hold.Feed(batch) {
			slog.Debug("hold "+action, "source", "x11")
			act("--" + action)
			if action == "send" {
				h.lockFocus()
			}
		}
		if active := hold.Active(); active && !grabbed {
			h.unlockFocus(false) // the hold's own keyboard grab takes over
			xproto.GrabKeyboard(h.conn, false, root, xproto.TimeCurrentTime, xproto.GrabModeAsync, xproto.GrabModeAsync).Reply()
		} else if grabbed && !active && !h.locked() { // Enter while held: the focus lock keeps the keyboard
			xproto.UngrabKeyboard(h.conn, xproto.TimeCurrentTime)
		}
		grabbed = hold.Active()
	}
}

// next returns the next queued event, if any; after a release (wait) it waits up to autoRepeatGap for one.
func next(events <-chan hotkey.Event, wait bool) (hotkey.Event, bool) {
	if !wait {
		select {
		case e, ok := <-events:
			return e, ok
		default:
			return hotkey.Event{}, false
		}
	}
	select {
	case e, ok := <-events:
		return e, ok
	case <-time.After(autoRepeatGap):
		return hotkey.Event{}, false
	}
}

// xembedTray is true when an XEmbed system tray (MATE's notification area, XFCE's systray) owns
// _NET_SYSTEM_TRAY_S<n>: Ayatana shows its fallback icon there. False on Wayland or without a display.
func xembedTray() bool {
	if desktop.Wayland() {
		return false
	}
	conn, err := xgb.NewConn()
	if err != nil {
		return false
	}
	defer conn.Close()
	name := fmt.Sprintf("_NET_SYSTEM_TRAY_S%d", conn.DefaultScreen)
	atom, err := xproto.InternAtom(conn, true, uint16(len(name)), name).Reply()
	if err != nil || atom.Atom == 0 {
		return false
	}
	owner, err := xproto.GetSelectionOwner(conn, atom.Atom).Reply()
	return err == nil && owner.Owner != 0
}
