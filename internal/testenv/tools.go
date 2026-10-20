package testenv

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func init() {
	fakes["xclip"] = clip
	fakes["wl-copy"] = clip
	fakes["wtype"] = keystroke
	fakes["ydotool"] = ydotool
}

// clip pretends to be xclip or wl-copy: it appends its stdin to "copied" and its command line to "copy-args".
func clip(args []string) int {
	name := filepath.Base(os.Args[0])
	if failing(name) {
		return 1
	}
	text, _ := io.ReadAll(os.Stdin)
	deliveryGate("HABLA_FAKE_COPY_GATE", "copy-started")
	appendLine("copied", string(text))
	appendLine("copy-args", strings.Join(append([]string{name}, args...), " "))
	return 0
}

// keystroke pretends to be wtype or ydotool: it appends its command line to "pasted".
func keystroke(args []string) int {
	name := filepath.Base(os.Args[0])
	if failing(name) {
		return 1
	}
	deliveryGate("HABLA_FAKE_PASTE_GATE", "paste-started")
	appendLine("pasted", strings.Join(append([]string{name}, args...), " "))
	return 0
}

// deliveryGate lets tests hold a desktop operation until its gate file is removed.
func deliveryGate(variable, started string) {
	path := os.Getenv(variable)
	if path == "" {
		return
	}
	appendLine(started, "ready")
	for {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// ydotool answers `ydotool help` like 1.x (exit 0), or like 0.1.x (exit 1) with HABLA_FAKE_YDOTOOL=0.1.
func ydotool(args []string) int {
	if len(args) > 0 && args[0] == "help" {
		if os.Getenv("HABLA_FAKE_YDOTOOL") == "0.1" {
			return 1
		}
		return 0
	}
	return keystroke(args)
}
