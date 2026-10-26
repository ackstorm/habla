package live

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func call(t *testing.T, files *tools, name string, args map[string]string) string {
	t.Helper()
	data, _ := json.Marshal(args)
	output, _ := files.call(name, string(data))
	return output
}

func TestFileToolsStayInTheirFolder(t *testing.T) {
	dir := t.TempDir()
	os.Symlink("..", filepath.Join(dir, "up"))
	files, err := openTools(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer files.root.Close()
	for _, path := range []string{"../outside.txt", "up/outside.txt", "/tmp/outside.txt"} {
		if out := call(t, files, "write_file", map[string]string{"path": path, "content": "x"}); !strings.HasPrefix(out, "error: ") {
			t.Errorf("write_file %s = %q, want an error", path, out)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "outside.txt")); err == nil {
		t.Fatal("a file was created outside the folder")
	}
}

func TestFileToolsChangeOnlyTheFilesTheyCreated(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "mine.md"), []byte("the user's own file"), 0o644)
	os.Symlink("mine.md", filepath.Join(dir, "link.md"))
	files, err := openTools(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer files.root.Close()
	const exists, notCreated = "error: the file already exists…", "error: not a file you created in this conversation"
	for _, step := range []struct {
		name string
		args map[string]string
		want string // the output, or its prefix when it ends in "…"
	}{
		{"write_file", map[string]string{"path": "mine.md", "content": "gone"}, exists},
		{"write_file", map[string]string{"path": "./mine.md", "content": "gone"}, exists},
		{"write_file", map[string]string{"path": "link.md", "content": "gone"}, exists},
		{"edit_file", map[string]string{"path": "mine.md", "old_text": "own", "new_text": "x"}, notCreated},
		{"delete_file", map[string]string{"path": "mine.md"}, notCreated},
		{"write_file", map[string]string{"path": "notes/summary.md", "content": "Hola, mundo."}, "wrote 12 bytes"},
		{"edit_file", map[string]string{"path": "notes/summary.md", "old_text": "mundo", "new_text": "Sevilla"}, "edited"},
		{"edit_file", map[string]string{"path": "notes/summary.md", "old_text": "nada", "new_text": "x"}, "error: old_text occurs 0 times…"},
		{"write_file", map[string]string{"path": "notes/summary.md", "content": "Adiós."}, "wrote 7 bytes"}, // its own
		{"delete_file", map[string]string{"path": "notes/summary.md"}, "deleted"},
		{"edit_file", map[string]string{"path": "notes/summary.md", "old_text": "Adiós", "new_text": "x"}, notCreated},
		{"read_file", map[string]string{"path": "mine.md"}, "error: unknown tool"},
	} {
		got := call(t, files, step.name, step.args)
		if prefix, ok := strings.CutSuffix(step.want, "…"); ok && strings.HasPrefix(got, prefix) || got == step.want {
			continue
		}
		t.Fatalf("%s %v = %q, want %q", step.name, step.args, got, step.want)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "mine.md")); string(data) != "the user's own file" {
		t.Fatalf("mine.md = %q", data)
	}
	if _, summary := files.call("write_file", `{"path":"a.txt","content":"private words"}`); summary != "write_file a.txt" {
		t.Fatalf("summary %q must name the tool and path, never the content", summary)
	}
}
