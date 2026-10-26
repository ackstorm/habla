package live

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// tools are habla live's file tools (on unless --no-tools): create text files in the folder habla live runs in, and
// edit or delete the files it created in this conversation; nothing else. os.Root keeps every path inside the folder,
// links included, and a file that existed before is never changed.
type tools struct {
	root    *os.Root
	created map[string]bool
}

func openTools(dir string) (*tools, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	return &tools{root: root, created: map[string]bool{}}, nil
}

// declarations are the tools in a session.update: flat function tools, as both realtime protocols take them.
func (t *tools) declarations() []map[string]any {
	fn := func(name, description string, params ...string) map[string]any {
		properties, required := map[string]any{}, []string{}
		for i := 0; i < len(params); i += 2 {
			properties[params[i]] = map[string]any{"type": "string", "description": params[i+1]}
			required = append(required, params[i])
		}
		return map[string]any{"type": "function", "name": name, "description": description,
			"parameters": map[string]any{"type": "object", "properties": properties, "required": required}}
	}
	const path = "Path relative to the user's current folder"
	return []map[string]any{
		fn("write_file", "Creates a new text file in the user's current folder, with any missing folders, or replaces "+
			"a file you created in this conversation. Existing files can't be changed.", "path", path,
			"content", "The whole content of the file"),
		fn("edit_file", "Replaces one exact piece of text in a file you created in this conversation.", "path", path,
			"old_text", "The text to replace, exactly as it is in the file; it must occur once", "new_text", "The replacement"),
		fn("delete_file", "Deletes a file you created in this conversation.", "path", path),
	}
}

// call runs one tool. output goes back to the model (an error too, as "error: …", so it can tell the user); summary
// is the transcript line, never the file's content.
func (t *tools) call(name, arguments string) (output, summary string) {
	var a struct {
		Path    string `json:"path"`
		Content string `json:"content"`
		OldText string `json:"old_text"`
		NewText string `json:"new_text"`
	}
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return "error: invalid arguments", name + ": invalid arguments"
	}
	path := filepath.Clean(a.Path)
	output, err := t.run(name, path, a.Content, a.OldText, a.NewText)
	if err != nil {
		return "error: " + err.Error(), fmt.Sprintf("%s %s: %v", name, path, err)
	}
	return output, name + " " + path
}

var errNotCreated = errors.New("not a file you created in this conversation")

func (t *tools) run(name, path, content, oldText, newText string) (string, error) {
	switch name {
	case "write_file":
		if _, err := t.root.Lstat(path); err == nil && !t.created[path] {
			return "", errors.New("the file already exists and can't be changed: pick another name")
		}
		if dir := filepath.Dir(path); dir != "." {
			if err := t.root.MkdirAll(dir, 0o755); err != nil {
				return "", err
			}
		}
		if err := t.root.WriteFile(path, []byte(content), 0o644); err != nil {
			return "", err
		}
		t.created[path] = true
		return fmt.Sprintf("wrote %d bytes", len(content)), nil
	case "edit_file":
		if !t.created[path] {
			return "", errNotCreated
		}
		data, err := t.root.ReadFile(path)
		if err != nil {
			return "", err
		}
		if n := strings.Count(string(data), oldText); oldText == "" || n != 1 {
			return "", fmt.Errorf("old_text occurs %d times; it must occur exactly once", n)
		}
		if err := t.root.WriteFile(path, []byte(strings.Replace(string(data), oldText, newText, 1)), 0o644); err != nil {
			return "", err
		}
		return "edited", nil
	case "delete_file":
		if !t.created[path] {
			return "", errNotCreated
		}
		if err := t.root.Remove(path); err != nil {
			return "", err
		}
		delete(t.created, path)
		return "deleted", nil
	}
	return "", errors.New("unknown tool")
}
