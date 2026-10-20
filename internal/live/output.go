package live

import (
	"os"
	"path/filepath"
	"time"

	"github.com/ackstorm/habla/internal/config"
)

func openTranscript(path string) (*os.File, error) {
	if path == "" {
		path = filepath.Join(filepath.Dir(config.Path()), "live_"+time.Now().Format("20060102_150405")+".txt")
	}
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
}
