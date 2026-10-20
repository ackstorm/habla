// Command habla is push-to-dictate for the Linux desktop through OpenAI-compatible or Gemini speech APIs.
package main

import (
	"fmt"
	"os"

	"github.com/ackstorm/habla/internal/listen"
	"github.com/ackstorm/habla/internal/live"
	"github.com/ackstorm/habla/internal/say"
	"github.com/ackstorm/habla/internal/stats"
	"github.com/ackstorm/habla/internal/tray"
)

// version is set at build time: -ldflags "-X main.version=0.1.0".
var version = "dev"

const usage = "usage: habla listen [--paste] [--provider NAME] [--start | --stop | --toggle | --cancel | --retry | --status]\n" +
	"       habla say [--out FILE | --stop] [--provider NAME] [TEXT]\n" +
	"       habla stats [--by day|week|month] [--json]\n" +
	"       habla live [--context FILE] [--no-output | --output-file FILE] [--provider NAME]\n" +
	"       habla tray [--install-autostart]\n" +
	"Every command takes --log LEVEL (error, warn, info, debug): the level of ~/.local/state/habla/habla.log for this run."

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "--version", "version":
			fmt.Println("habla " + version)
			os.Exit(0)
		case "--help", "-h":
			fmt.Println(usage)
			os.Exit(0)
		case "listen":
			os.Exit(listen.Main(os.Args[2:]))
		case "say":
			os.Exit(say.Main(os.Args[2:]))
		case "stats":
			os.Exit(stats.Main(os.Args[2:]))
		case "live":
			os.Exit(live.Main(os.Args[2:]))
		case "tray":
			os.Exit(tray.Main(os.Args[2:]))
		}
	}
	fmt.Fprintln(os.Stderr, usage)
	os.Exit(2)
}
