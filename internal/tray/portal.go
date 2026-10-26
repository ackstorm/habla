package tray

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/ackstorm/habla/internal/desktop"
	"github.com/ackstorm/habla/internal/hotkey"
	"github.com/godbus/dbus/v5"
)

const (
	portalName = "org.freedesktop.portal.Desktop"
	portalPath = "/org/freedesktop/portal/desktop"
	shortcuts  = "org.freedesktop.portal.GlobalShortcuts"
	shortcutID = "dictate" // with AppID, what the desktop remembers the confirmed key under. Never change it.
)

// portalKeys are the shortcuts' Hold keycodes by ID: dictate, and send and cancel (the hotkey's modifiers + Return or
// Escape, so with the hotkey held its Enter sends and its Esc discards). Never change the IDs.
var portalKeys = map[string]int{shortcutID: hotkey.PortalKey, "send": hotkey.PortalSend, "cancel": hotkey.PortalCancel}

// callTimeout bounds each portal method call, as GDBus's default (the old code's -1) does. Not the Response: the
// user may take any time over the dialog. A var for tests.
var callTimeout = 25 * time.Second

// shortcut is one entry of BindShortcuts' a(sa{sv}).
type shortcut struct {
	ID      string
	Options map[string]dbus.Variant
}

type portal struct {
	conn    *dbus.Conn
	obj     dbus.BusObject
	signals chan *dbus.Signal
	tokens  int
}

// runPortal is KDE/GNOME Wayland hold-to-talk through the XDG GlobalShortcuts portal:
// Activated = press, Deactivated = release; the hotkey's modifiers + Return sends, + Escape discards (Hold). A hotkey
// without modifiers gets neither: a bare Enter would be taken from every app. The desktop asks once to confirm the
// keys (trigger is only its suggestion) and remembers them per AppID and shortcut ID. It calls
// act("--start" | "--stop" | "--send" | "--cancel") for the life of the process, from its own goroutine; it waits on
// D-Bus and on the user's dialog. A failure notifies once and returns.
func runPortal(trigger string, act func(flag string)) {
	// A connection of its own: Register must precede every other portal call on it.
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		portalMissing(err)
		return
	}
	defer conn.Close()
	p := &portal{conn: conn, obj: conn.Object(portalName, portalPath), signals: make(chan *dbus.Signal, 16)}
	conn.Signal(p.signals)
	if _, err := InstallDesktopEntry(); err != nil { // the portal finds the app ID through it
		fmt.Fprintln(os.Stderr, "habla tray:", err)
	}
	// Portals before 1.19 have no Registry and take host apps without an app ID; else CreateSession fails.
	p.call("org.freedesktop.host.portal.Registry.Register", AppID, map[string]dbus.Variant{})
	if err := p.call("org.freedesktop.DBus.Properties.Get", shortcuts, "version"); err != nil {
		portalMissing(err)
		return
	}
	results, err := p.request("CreateSession", map[string]dbus.Variant{"session_handle_token": dbus.MakeVariant("habla")})
	if err != nil {
		portalFailed(err.Error())
		return
	}
	session := dbus.ObjectPath(fmt.Sprint(results["session_handle"].Value())) // "s" in the spec, "o" in some portals
	for _, signal := range []string{"Activated", "Deactivated"} {
		conn.AddMatchSignal(dbus.WithMatchSender(portalName), dbus.WithMatchObjectPath(portalPath),
			dbus.WithMatchInterface(shortcuts), dbus.WithMatchMember(signal))
	}
	bind := func(id, description, trigger string) shortcut {
		return shortcut{id, map[string]dbus.Variant{"description": dbus.MakeVariant(description),
			"preferred_trigger": dbus.MakeVariant(trigger)}}
	}
	want := []shortcut{bind(shortcutID, "Hold to dictate", trigger)}
	if i := strings.LastIndex(trigger, "+"); i > 0 {
		for _, s := range [][3]string{{"send", "Return", "Send the dictation (paste, then Enter)"},
			{"cancel", "Escape", "Discard the dictation"}} {
			if mods := trigger[:i+1]; mods+s[1] != trigger {
				want = append(want, bind(s[0], s[2], mods+s[1]))
			}
		}
	}
	results, err = p.request("BindShortcuts", session, want, "", map[string]dbus.Variant{})
	if err != nil {
		portalFailed(err.Error())
		return
	}
	var bound []shortcut
	if v, ok := results["shortcuts"]; ok {
		if err := dbus.Store([]any{v.Value()}, &bound); err != nil {
			portalFailed("BindShortcuts: " + err.Error())
			return
		}
	}
	if !slices.ContainsFunc(bound, func(s shortcut) bool { return s.ID == shortcutID }) {
		portalFailed("the shortcut was declined")
		return
	}
	hold := hotkey.NewHold(hotkey.PortalKey, hotkey.PortalCancel).WithEnter(0, hotkey.PortalSend)
	for s := range p.signals { // closed with the connection
		name := strings.TrimPrefix(s.Name, shortcuts+".")
		if (name != "Activated" && name != "Deactivated") || s.Path != portalPath || len(s.Body) < 3 ||
			s.Body[0] != session {
			continue
		}
		id, _ := s.Body[1].(string)
		key, ok := portalKeys[id]
		if !ok {
			continue
		}
		timestamp, _ := s.Body[2].(uint64)
		for _, action := range hold.Feed([]hotkey.Event{hotkey.PortalEvent(name, key, uint32(timestamp))}) {
			slog.Debug("hold "+action, "source", "portal")
			act("--" + action)
		}
	}
}

// request calls a portal method that answers with a Request.Response signal, and returns its results. The last of
// args is the options, which get the handle_token.
func (p *portal) request(method string, args ...any) (map[string]dbus.Variant, error) {
	p.tokens++
	token := fmt.Sprintf("habla%d", p.tokens)
	sender := strings.ReplaceAll(strings.TrimPrefix(p.conn.Names()[0], ":"), ".", "_")
	path := dbus.ObjectPath(portalPath + "/request/" + sender + "/" + token)
	match := []dbus.MatchOption{dbus.WithMatchSender(portalName), dbus.WithMatchObjectPath(path),
		dbus.WithMatchInterface("org.freedesktop.portal.Request"), dbus.WithMatchMember("Response")}
	// Subscribed before the call, so a fast Response is not missed.
	if err := p.conn.AddMatchSignal(match...); err != nil {
		return nil, err
	}
	defer p.conn.RemoveMatchSignal(match...)
	args[len(args)-1].(map[string]dbus.Variant)["handle_token"] = dbus.MakeVariant(token)
	if err := p.call(shortcuts+"."+method, args...); err != nil {
		return nil, err
	}
	for s := range p.signals {
		if s.Path != path || s.Name != "org.freedesktop.portal.Request.Response" || len(s.Body) != 2 {
			continue
		}
		code, _ := s.Body[0].(uint32)
		switch code {
		case 0:
			results, _ := s.Body[1].(map[string]dbus.Variant)
			return results, nil
		case 1:
			return nil, errors.New(method + " cancelled") // by the user
		}
		return nil, errors.New(method + " failed")
	}
	return nil, errors.New("the session bus closed")
}

// call is a method call bounded by callTimeout.
func (p *portal) call(method string, args ...any) error {
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	return p.obj.CallWithContext(ctx, method, 0, args...).Err
}

// portalMissing: no session bus or no GlobalShortcuts portal; the toggle shortcut of v1 (README) still works.
func portalMissing(err error) {
	desktop.Notify("Hold-to-talk needs the GlobalShortcuts portal: bind habla listen --toggle instead", err.Error(), false)
}

func portalFailed(reason string) {
	desktop.Notify("Hold-to-talk is off: "+reason, "Bind habla listen --toggle to a shortcut (see the README).", true)
}
