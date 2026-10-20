package testenv

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
)

func init() {
	fakes["pw-play"] = player
	fakes["paplay"] = player
}

// player pretends to be pw-play or paplay: it appends "name args first-bytes-of-the-file" to "played" (and "started"
// before reading the file). With HABLA_FAKE_PLAY=block it then waits for SIGTERM, appends "terminated" to "player"
// and exits.
func player(args []string) int {
	name := filepath.Base(os.Args[0])
	term := make(chan os.Signal, 1)
	signal.Notify(term, syscall.SIGTERM)
	appendLine("started", name)
	if failing(name) {
		return 3
	}
	if slices.Contains(args, "--raw") {
		f, err := os.OpenFile(filepath.Join(os.Getenv("HABLA_FAKE_DIR"), "raw-played"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return 1
		}
		defer f.Close()
		_, err = io.Copy(f, os.Stdin)
		if err != nil {
			return 1
		}
		return 0
	}
	data, _ := os.ReadFile(args[len(args)-1])
	appendLine("played", fmt.Sprintf("%s %s %s", name, strings.Join(args, " "), data[:min(len(data), 16)]))
	if os.Getenv("HABLA_FAKE_PLAY") == "block" {
		<-term
		appendLine("player", "terminated")
		return 1
	}
	return 0
}
