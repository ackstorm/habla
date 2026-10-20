package release

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ackstorm/habla/internal/config"
)

// examples/config.toml must load as a real config.toml.
func TestExampleConfig(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "examples", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := os.MkdirAll(filepath.Dir(config.Path()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.Path(), data, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("examples/config.toml rejected by Habla: %v", err)
	}
	if cfg.Split != config.Defaults().Split {
		t.Fatalf("example split settings differ from application defaults: %+v", cfg.Split)
	}
	if cfg.STT.API != "gemini" || cfg.LLM.API != "openai" || cfg.TTS.API != "elevenlabs" {
		t.Errorf("active providers: stt %s, llm %s, tts %s", cfg.STT.API, cfg.LLM.API, cfg.TTS.API)
	}
}
