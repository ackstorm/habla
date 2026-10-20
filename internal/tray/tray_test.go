package tray

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ackstorm/habla/internal/testenv"
)

func TestInstallAutostartFlagWritesTheEntriesAndIcons(t *testing.T) {
	env := testenv.Setup(t)
	t.Setenv("XDG_DATA_HOME", filepath.Join(env.Dir, "data"))
	if code := Main([]string{"--install-autostart"}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	for _, path := range []string{
		filepath.Join(env.Dir, "config", "autostart", "habla.desktop"),
		filepath.Join(env.Dir, "data", "applications", AppID+".desktop"),
		filepath.Join(env.Dir, "data", "icons", "hicolor", "scalable", "status", "habla-recording-symbolic.svg"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Error(err)
		}
	}
}

func TestTrayAlreadyRunning(t *testing.T) {
	testenv.Setup(t)
	lock, err := AcquireLock()
	if err != nil || lock == nil {
		t.Fatalf("lock: %v, %v", lock, err)
	}
	defer lock.Close()
	if code := Main(nil); code != 1 { // before GTK: the lock comes first
		t.Fatalf("exit %d", code)
	}
}

func TestInvalidConfigStopsTheTray(t *testing.T) {
	e := testenv.Setup(t)
	path := filepath.Join(e.Dir, "config", "habla", "config.toml")
	os.MkdirAll(filepath.Dir(path), 0o755)
	os.WriteFile(path, []byte("[tray]\nhotkey = 1\n"), 0o644)
	code := -1
	out := testenv.Stderr(t, func() { code = Main(nil) })
	if code != 1 || !strings.HasPrefix(out, "Invalid config: "+path+": toml: line 2") {
		t.Fatalf("exit %d, stderr %q", code, out)
	}
}

func TestWatchReportsFileEvents(t *testing.T) {
	dir := t.TempDir()
	changed := make(chan struct{}, 10)
	if err := watch([]string{dir}, func() { changed <- struct{}{} }); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "state"), []byte("recording\n"), 0o644)
	select {
	case <-changed:
	case <-time.After(5 * time.Second):
		t.Fatal("no event within 5 s")
	}
}
