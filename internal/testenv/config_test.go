package testenv

import (
	"strings"
	"testing"
)

func TestMergeOverlaysSectionsAndKeys(t *testing.T) {
	base := "[general]\nmax_seconds = 300\npaste = false\n\n[stt]\nmodel = \"a\"\n"
	extra := "[general]\nmax_seconds = 30\n[split]\nenabled = true\n[stt]\ntransform = \"\"\n"
	got := Merge(base, extra)
	if got == base || got == extra {
		t.Fatalf("merge did not combine both configs: %q", got)
	}
	for _, want := range []string{"max_seconds = 30", "paste = false", "model = \"a\"", "transform = \"\"", "[split]", "enabled = true"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q from %s", want, got)
		}
	}
}
