package release

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ackstorm/habla/internal/config"
)

func TestConfigurationPlugin(t *testing.T) {
	root := filepath.Join("..", "..")
	var marketplace struct {
		Plugins []struct{ Name, Source string }
	}
	data, err := os.ReadFile(filepath.Join(root, ".claude-plugin", "marketplace.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &marketplace); err != nil {
		t.Fatal(err)
	}
	if len(marketplace.Plugins) != 1 {
		t.Fatalf("want one shared plugin, got %d", len(marketplace.Plugins))
	}
	plugin := marketplace.Plugins[0]
	dir := filepath.Join(root, plugin.Source)
	for _, manifest := range []string{"plugin.json", ".claude-plugin/plugin.json"} {
		data, err := os.ReadFile(filepath.Join(dir, manifest))
		if err != nil {
			t.Fatal(err)
		}
		var m struct{ Name, Version string }
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatal(err)
		}
		if m.Name != plugin.Name || m.Version == "" {
			t.Fatalf("%s: inconsistent plugin identity", manifest)
		}
	}
	skill := filepath.Join(dir, "skills", "habla")
	if _, err := os.Stat(filepath.Join(skill, "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(filepath.Join(skill, "references", "configuration.md"))
	if err != nil {
		t.Fatal(err)
	}
	_, rest, found := strings.Cut(string(data), "```toml\n")
	example, _, closed := strings.Cut(rest, "\n```")
	if !found || !closed {
		t.Fatal("missing complete TOML example")
	}
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := os.MkdirAll(filepath.Dir(config.Path()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.Path(), []byte(example), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("plugin example rejected by Habla: %v", err)
	}
	if cfg.Split != config.Defaults().Split {
		t.Fatalf("plugin split settings differ from application defaults: %+v", cfg.Split)
	}
}
