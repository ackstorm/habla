package logs

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDeltaBetweenLogEntries(t *testing.T) {
	var tty bytes.Buffer
	h := &handler{prefix: "listen[123]", path: filepath.Join(t.TempDir(), "habla.log"), tty: &tty}
	for i := range 2 {
		if i > 0 {
			time.Sleep(15 * time.Millisecond)
		}
		r := slog.NewRecord(time.Now(), slog.LevelInfo, "chunk", 0)
		r.Add("bytes", 1366, "ms", 1350)
		if err := h.Handle(context.Background(), r); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(h.path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != tty.String() {
		t.Fatal("file and terminal log differ")
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines", len(lines))
	}
	var previous time.Time
	for i, line := range lines {
		fields := strings.Fields(line)
		if len(fields) != 7 || fields[3] != "[delta_ms=0]" && i == 0 || !strings.HasPrefix(fields[3], "[delta_ms=") || !strings.Contains(line, "chunk bytes=1366 ms=1350") {
			t.Fatalf("unexpected log format: %s", line)
		}
		delta, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(fields[3], "[delta_ms="), "]"), 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		stamp, err := time.Parse("2006-01-02T15:04:05.000", fields[0])
		if err != nil {
			t.Fatal(err)
		}
		if i > 0 && (delta < 15 || delta-stamp.Sub(previous).Milliseconds() < -1 || delta-stamp.Sub(previous).Milliseconds() > 1) {
			t.Fatalf("delta %d does not match timestamp gap %s", delta, stamp.Sub(previous))
		}
		previous = stamp
	}
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			r := slog.NewRecord(time.Now(), slog.LevelInfo, "concurrent", 0)
			if err := h.Handle(context.Background(), r); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if strings.Count(tty.String(), " concurrent\n") != 16 || strings.Contains(tty.String(), "[delta_ms=-") {
		t.Fatal("concurrent entries were lost or have negative gaps")
	}
}
