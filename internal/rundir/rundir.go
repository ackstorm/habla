// Package rundir holds the files in $XDG_RUNTIME_DIR/habla/ through which habla processes coordinate:
// listen.pid, say.pid, state, current.wav and failed.wav.
package rundir

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

const (
	Listen = "listen.pid"
	Say    = "say.pid"
)

var states = []string{"recording", "busy", "transform", "failed"}

// Processing reports whether state is a transcription or transform phase: a dictation can be continued or discarded.
func Processing(state string) bool { return state == "busy" || state == "transform" }

// SendMark is the file `habla listen --send` (Enter while busy) leaves for the busy session: its delivery pastes and
// presses Enter.
func SendMark() string { return filepath.Join(Dir(), "send") }

// Dir is $XDG_RUNTIME_DIR/habla (in RAM, private to the user), created on first use.
func Dir() string {
	base := os.Getenv("XDG_RUNTIME_DIR")
	if base == "" {
		base = fmt.Sprintf("/run/user/%d", os.Getuid())
	}
	dir := filepath.Join(base, "habla")
	os.MkdirAll(dir, 0o700) // ponytail: a failure surfaces at the first file operation in it
	return dir
}

// Alive reports whether pid runs the habla binary: a PID reused by another program is not, nor is a zombie (its
// cmdline is empty).
func Alive(pid int) bool {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return false
	}
	for _, arg := range strings.Split(string(data), "\x00") {
		if filepath.Base(arg) == "habla" {
			return true
		}
	}
	return false
}

// RunningPID returns the PID in name (Listen or Say) while that process runs, else 0.
func RunningPID(name string) int {
	data, err := os.ReadFile(filepath.Join(Dir(), name))
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || !Alive(pid) {
		return 0
	}
	return pid
}

// Claim takes name (Listen or Say) for this process; false means a live habla process holds it. initialState, when
// not "", is published before the PID becomes visible, so a --stop or --cancel arriving while the recorder starts
// already finds a recording to signal.
func Claim(name, initialState string) (bool, error) {
	dir := Dir()
	// Lock the stable directory inode: stale-file removal and publication must be one operation.
	lock, err := os.Open(dir)
	if err != nil {
		return false, err
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		return false, err
	}
	if RunningPID(name) != 0 {
		return false, nil
	}
	path := filepath.Join(dir, name)
	os.Remove(path)
	if initialState != "" {
		WriteState(initialState)
	}
	return createExclusive(path, fmt.Sprintf("%d\n", os.Getpid()))
}

// Release removes name if it holds our PID.
func Release(name string) {
	path := filepath.Join(Dir(), name)
	if data, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(data)) == strconv.Itoa(os.Getpid()) {
		os.Remove(path)
	}
}

// ReadState returns the recording/delivery phase. Active phases left by a dead process read idle.
func ReadState() string {
	data, err := os.ReadFile(filepath.Join(Dir(), "state"))
	if err != nil {
		return "idle"
	}
	fields := strings.Fields(string(data))
	if len(fields) >= 2 && slices.Contains([]string{"copying", "pasting", "copy_failed"}, fields[0]) {
		pid, err := strconv.Atoi(fields[1])
		if err == nil && pid > 0 && (fields[0] == "copy_failed" || Alive(pid)) {
			return fields[0]
		}
		return "idle"
	}
	s := strings.TrimSpace(string(data))
	if !slices.Contains(states, s) || (s != "failed" && RunningPID(Listen) == 0) {
		return "idle"
	}
	return s
}

// Delivery updates only our delivery phase. Claim uses the same lock, so an older clipboard operation can never
// overwrite or clear a newer recording. A copy failure stays until the next recording, like a transcription failure.
func Delivery(phase string) {
	lock, err := os.Open(Dir())
	if err != nil {
		return
	}
	defer lock.Close()
	if unix.Flock(int(lock.Fd()), unix.LOCK_EX) != nil {
		return
	}
	pid := strconv.Itoa(os.Getpid())
	data, _ := os.ReadFile(filepath.Join(Dir(), "state"))
	fields := strings.Fields(string(data))
	tail := "" // " transform" after a transform, kept through pasting: the pill keeps its colour
	if phase == "copying" {
		holder, _ := os.ReadFile(filepath.Join(Dir(), Listen))
		if len(fields) != 1 || !Processing(fields[0]) || (len(holder) > 0 && strings.TrimSpace(string(holder)) != pid) {
			return
		}
		if fields[0] == "transform" {
			tail = " transform"
		}
	} else if len(fields) < 2 || fields[1] != pid || !slices.Contains([]string{"copying", "pasting"}, fields[0]) {
		return
	} else if phase == "pasting" && AfterTransform() {
		tail = " transform"
	}
	if phase == "" {
		WriteState("")
	} else {
		WriteState(phase + " " + pid + tail)
	}
}

// AfterTransform reports whether the current delivery (copying or pasting) follows a transform.
func AfterTransform() bool {
	data, _ := os.ReadFile(filepath.Join(Dir(), "state"))
	fields := strings.Fields(string(data))
	return len(fields) == 3 && fields[2] == "transform"
}

// WriteState writes state atomically; Delivery adds its owner PID. "" removes it (idle).
// ponytail: no error; state only drives the icon, and a failed write never loses audio.
func WriteState(state string) {
	path := filepath.Join(Dir(), "state")
	if state == "" {
		os.Remove(path)
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".state-*")
	if err != nil {
		return
	}
	_, err = tmp.WriteString(state + "\n")
	if tmp.Close() != nil || err != nil || os.Rename(tmp.Name(), path) != nil {
		os.Remove(tmp.Name())
	}
}

// createExclusive creates path with content, failing if it exists. Readers never see it empty (unlike O_EXCL + write).
func createExclusive(path, content string) (bool, error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".claim-*")
	if err != nil {
		return false, err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	if err := os.Link(tmp.Name(), path); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
