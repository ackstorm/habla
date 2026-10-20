package testenv

import (
	"fmt"
	"os"
	"strings"
	"sync/atomic"

	"github.com/godbus/dbus/v5"
)

const portalPath = "/org/freedesktop/portal/desktop"

func init() { fakes["fake-portal"] = fakePortal }

// shortcut is one entry of BindShortcuts' a(sa{sv}).
type shortcut struct {
	ID      string
	Options map[string]dbus.Variant
}

// portalKeys follow a bound shortcut: a press, a repeated press, a release, a second press and release, then a key
// for another shortcut and one for another session (session set).
var portalKeys = []struct {
	signal, id string
	time       uint64
	session    dbus.ObjectPath
}{{"Activated", "dictate", 10, ""}, {"Activated", "dictate", 20, ""}, {"Deactivated", "dictate", 900, ""},
	{"Activated", "dictate", 1000, ""}, {"Deactivated", "dictate", 1500, ""}, {"Activated", "other", 1600, ""},
	{"Activated", "dictate", 1700, portalPath + "/session/other/habla"}}

type portal struct {
	conn *dbus.Conn
	mode string
}

// fakePortal is xdg-desktop-portal with a GlobalShortcuts backend, run under dbus-run-session. $HABLA_FAKE_PORTAL
// picks what BindShortcuts does: "bind" (the user confirms; then portalKeys), "decline" (code 1), "fail" (code 2),
// "stall" (never replies), or "none": no GlobalShortcuts interface. It appends each call to "portal". It is also the
// notification daemon: each Notify and CloseNotification goes to "notifications". It prints the bus address once it
// owns both names, and serves until killed.
func fakePortal(args []string) int {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	p := &portal{conn, os.Getenv("HABLA_FAKE_PORTAL")}
	// ponytail: one value on every interface, so each exposes all the methods; harmless in a fake.
	conn.Export(p, portalPath, "org.freedesktop.host.portal.Registry")
	conn.Export(p, portalPath, "org.freedesktop.DBus.Properties")
	if p.mode != "none" {
		conn.Export(p, portalPath, "org.freedesktop.portal.GlobalShortcuts")
	}
	conn.Export(&notifications{}, "/org/freedesktop/Notifications", "org.freedesktop.Notifications")
	for _, name := range []string{"org.freedesktop.portal.Desktop", "org.freedesktop.Notifications"} {
		if reply, err := conn.RequestName(name, dbus.NameFlagDoNotQueue); err != nil ||
			reply != dbus.RequestNameReplyPrimaryOwner {
			fmt.Fprintln(os.Stderr, "fake-portal: could not own", name, err)
			return 1
		}
	}
	fmt.Println(os.Getenv("DBUS_SESSION_BUS_ADDRESS"))
	select {}
}

// escape is the sender part of the request and session paths: ":1.42" → "1_42".
func escape(sender dbus.Sender) string {
	return strings.ReplaceAll(strings.TrimPrefix(string(sender), ":"), ".", "_")
}

func (p *portal) Register(app string, options map[string]dbus.Variant) *dbus.Error {
	appendLine("portal", "Register "+app)
	return nil
}

func (p *portal) Get(iface, name string) (dbus.Variant, *dbus.Error) {
	appendLine("portal", "Get "+iface+" "+name)
	if p.mode == "none" {
		return dbus.Variant{}, &dbus.Error{Name: "org.freedesktop.DBus.Error.InvalidArgs",
			Body: []any{"No such interface “" + iface + "”"}}
	}
	return dbus.MakeVariant(uint32(1)), nil
}

func (p *portal) CreateSession(sender dbus.Sender, options map[string]dbus.Variant) (dbus.ObjectPath, *dbus.Error) {
	appendLine("portal", "CreateSession")
	request := p.request(sender, options)
	session := portalPath + "/session/" + escape(sender) + "/" + fmt.Sprint(options["session_handle_token"].Value())
	go p.respond(request, 0, map[string]dbus.Variant{"session_handle": dbus.MakeVariant(session)})
	return request, nil
}

func (p *portal) BindShortcuts(sender dbus.Sender, session dbus.ObjectPath, shortcuts []shortcut, parent string,
	options map[string]dbus.Variant) (dbus.ObjectPath, *dbus.Error) {
	for _, s := range shortcuts {
		appendLine("portal", fmt.Sprintf("BindShortcuts %s %v %v", s.ID, s.Options["preferred_trigger"].Value(),
			s.Options["description"].Value()))
	}
	request := p.request(sender, options)
	switch p.mode {
	case "stall":
		select {} // a hung backend
	case "decline":
		go p.respond(request, 1, map[string]dbus.Variant{})
	case "fail":
		go p.respond(request, 2, map[string]dbus.Variant{})
	default:
		go func() {
			p.respond(request, 0, map[string]dbus.Variant{"shortcuts": dbus.MakeVariant(shortcuts)})
			for _, k := range portalKeys {
				if k.session == "" {
					k.session = session
				}
				p.conn.Emit(portalPath, "org.freedesktop.portal.GlobalShortcuts."+k.signal, k.session, k.id, k.time,
					map[string]dbus.Variant{})
			}
		}()
	}
	return request, nil
}

// request is the Request object path the caller computed from its handle_token.
func (p *portal) request(sender dbus.Sender, options map[string]dbus.Variant) dbus.ObjectPath {
	return dbus.ObjectPath(portalPath + "/request/" + escape(sender) + "/" + fmt.Sprint(options["handle_token"].Value()))
}

func (p *portal) respond(request dbus.ObjectPath, code uint32, results map[string]dbus.Variant) {
	p.conn.Emit(request, "org.freedesktop.portal.Request.Response", code, results)
}

type notifications struct{ last atomic.Uint32 }

func (n *notifications) Notify(app string, replaces uint32, icon, summary, body string, actions []string,
	hints map[string]dbus.Variant, timeout int32) (uint32, *dbus.Error) {
	id := n.last.Add(1)
	urgency, _ := hints["urgency"].Value().(byte)
	appendLine("notifications", fmt.Sprintf("Notify %d %s|%s urgency=%d", id, summary, body, urgency))
	return id, nil
}

func (n *notifications) CloseNotification(id uint32) *dbus.Error {
	appendLine("notifications", fmt.Sprintf("Close %d", id))
	return nil
}
