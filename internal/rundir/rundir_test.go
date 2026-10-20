package rundir_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/ackstorm/habla/internal/rundir"
	"github.com/ackstorm/habla/internal/testenv"
)

func writePID(t *testing.T, name string, pid int) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(rundir.Dir(), name), []byte(fmt.Sprintf("%d\n", pid)), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(name string) string {
	data, _ := os.ReadFile(filepath.Join(rundir.Dir(), name))
	return string(data)
}

func TestDirIsPrivate(t *testing.T) {
	testenv.Setup(t)
	info, err := os.Stat(rundir.Dir())
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("%v %v", info.Mode(), err)
	}
}

func TestClaimWritesOurPIDAndReleaseRemovesIt(t *testing.T) {
	testenv.Setup(t)
	if ok, err := rundir.Claim(rundir.Listen, ""); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if got := read(rundir.Listen); got != fmt.Sprintf("%d\n", os.Getpid()) {
		t.Fatalf("%q", got)
	}
	rundir.Release(rundir.Listen)
	if _, err := os.Stat(filepath.Join(rundir.Dir(), rundir.Listen)); !os.IsNotExist(err) {
		t.Fatal("listen.pid left behind")
	}
}

func TestClaimFailsWhileAHablaProcessHoldsTheFile(t *testing.T) {
	testenv.Setup(t)
	holder := testenv.HablaProcess(t)
	writePID(t, rundir.Listen, holder.Process.Pid)
	if ok, err := rundir.Claim(rundir.Listen, "recording"); ok || err != nil {
		t.Fatal(ok, err)
	}
	if rundir.RunningPID(rundir.Listen) != holder.Process.Pid {
		t.Fatal("holder not seen")
	}
	if read("state") != "" {
		t.Fatal("a failed claim must not publish a state")
	}
}

func TestAPIDReusedByAnotherProgramIsStale(t *testing.T) {
	testenv.Setup(t)
	other := exec.Command("/bin/sleep", "60") // running, but not habla
	if err := other.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { other.Process.Kill(); other.Wait() })
	writePID(t, rundir.Listen, other.Process.Pid)
	if rundir.RunningPID(rundir.Listen) != 0 {
		t.Fatal("a non-habla process counted as running")
	}
	if ok, err := rundir.Claim(rundir.Listen, ""); !ok || err != nil {
		t.Fatal(ok, err)
	}
}

func TestClaimPublishesTheInitialState(t *testing.T) {
	testenv.Setup(t)
	if ok, _ := rundir.Claim(rundir.Listen, "recording"); !ok {
		t.Fatal("claim failed")
	}
	if read("state") != "recording\n" {
		t.Fatalf("%q", read("state"))
	}
}

func TestReleaseLeavesAnotherProcessFileAlone(t *testing.T) {
	testenv.Setup(t)
	writePID(t, rundir.Listen, 1)
	rundir.Release(rundir.Listen)
	if read(rundir.Listen) != "1\n" {
		t.Fatal("removed another process's file")
	}
}

func TestStateRoundTripAndIdleWhenAbsent(t *testing.T) {
	testenv.Setup(t)
	if rundir.ReadState() != "idle" {
		t.Fatal(rundir.ReadState())
	}
	rundir.WriteState("failed") // failed needs no live process
	if rundir.ReadState() != "failed" {
		t.Fatal(rundir.ReadState())
	}
	rundir.WriteState("")
	if rundir.ReadState() != "idle" || read("state") != "" {
		t.Fatal("state not removed")
	}
}

func TestActiveProcessingNeedsALiveProcess(t *testing.T) {
	testenv.Setup(t)
	pid := testenv.HablaProcess(t).Process.Pid
	for _, phase := range []string{"recording", "busy", "transform"} {
		writePID(t, rundir.Listen, 999999999)
		rundir.WriteState(phase)
		if rundir.ReadState() != "idle" {
			t.Fatalf("%s left by a dead process must read idle", phase)
		}
		writePID(t, rundir.Listen, pid)
		if got := rundir.ReadState(); got != phase {
			t.Fatalf("got %q, want %q", got, phase)
		}
	}
}

func TestTransformHandsOffToDelivery(t *testing.T) {
	testenv.Setup(t)
	rundir.WriteState("transform")
	rundir.Delivery("copying")
	if got := read("state"); got != fmt.Sprintf("copying %d transform\n", os.Getpid()) || !rundir.AfterTransform() {
		t.Fatalf("transform did not hand off to clipboard delivery: %q", got)
	}
	rundir.Delivery("pasting")
	if got := read("state"); got != fmt.Sprintf("pasting %d transform\n", os.Getpid()) {
		t.Fatalf("pasting lost the transform: %q", got)
	}
	rundir.Delivery("")
	if got := read("state"); got != "" {
		t.Fatalf("completed delivery must clear the pill, got %q", got)
	}
}

func TestDeliveryStateOutlivesTheRecordingSlot(t *testing.T) {
	testenv.Setup(t)
	holder := testenv.HablaProcess(t)
	for _, state := range []string{"copying", "pasting"} {
		rundir.WriteState(fmt.Sprintf("%s %d", state, holder.Process.Pid))
		if got := rundir.ReadState(); got != state {
			t.Fatalf("released recording slot: state = %q, want %q", got, state)
		}
		rundir.WriteState(fmt.Sprintf("%s %d transform", state, holder.Process.Pid))
		if got := rundir.ReadState(); got != state {
			t.Fatalf("after transform: state = %q, want %q", got, state)
		}
		rundir.WriteState(state + " 999999999")
		if got := rundir.ReadState(); got != "idle" {
			t.Fatalf("dead delivery process: state = %q", got)
		}
	}
	rundir.WriteState("copy_failed 999999999")
	if got := rundir.ReadState(); got != "copy_failed" {
		t.Fatalf("copy failure must remain visible, got %q", got)
	}
}

func TestOlderDeliveryCannotOverwriteANewRecording(t *testing.T) {
	testenv.Setup(t)
	rundir.WriteState("busy")
	rundir.Delivery("copying")
	if ok, err := rundir.Claim(rundir.Listen, "recording"); err != nil || !ok {
		t.Fatal(ok, err)
	}
	for _, phase := range []string{"pasting", "copy_failed", ""} {
		rundir.Delivery(phase)
		if got := read("state"); got != "recording\n" {
			t.Fatalf("older delivery %q replaced the new recording: %q", phase, got)
		}
	}
}

func TestCopyFailureIsNotClearedByDeliveryTransform(t *testing.T) {
	testenv.Setup(t)
	rundir.WriteState("busy")
	rundir.Delivery("copying")
	if rundir.AfterTransform() {
		t.Fatal("a delivery without transform reads as after transform")
	}
	rundir.Delivery("copy_failed")
	rundir.Delivery("")
	if got := rundir.ReadState(); got != "copy_failed" {
		t.Fatalf("copy failure was hidden by transform: %q", got)
	}
}
