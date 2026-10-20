package tray

import (
	"errors"

	"golang.org/x/sys/unix"
)

// Files created, removed, renamed or closed after writing. No IN_MODIFY: the recorder writes current.wav all the
// time, and the files the tray reads (state, the PID files, failed.wav, config.toml) all end with one of these.
const watchMask = unix.IN_CREATE | unix.IN_DELETE | unix.IN_MOVED_FROM | unix.IN_MOVED_TO | unix.IN_CLOSE_WRITE

// watch calls changed from a goroutine after each batch of inotify events in dirs, for the life of the process.
func watch(dirs []string, changed func()) error {
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC)
	if err != nil {
		return err
	}
	for _, dir := range dirs {
		if _, err := unix.InotifyAddWatch(fd, dir, watchMask); err != nil {
			unix.Close(fd)
			return err
		}
	}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := unix.Read(fd, buf)
			if errors.Is(err, unix.EINTR) {
				continue
			}
			if err != nil {
				return
			}
			if n > 0 {
				changed()
			}
		}
	}()
	return nil
}
