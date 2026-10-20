package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestTableNames(t *testing.T) {
	for _, name := range []string{"AZaz09_-", "", "with space", "a.b", "café", "a\nb", "a\x00b"} {
		_, err := parse(fmt.Sprintf("[styles.%q]\ninstructions = \"Speak calmly.\"\n", name))
		if (err == nil) != (name == "AZaz09_-") {
			t.Errorf("name %q: %v", name, err)
		}
	}
}

func TestUploadInterval(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  int
	}{
		{"", 50}, {"50", 50}, {"100", 100}, {"200", 200}, {"1000", 1000},
		{"0", 0}, {"49", 0}, {"1001", 0}, {"-1", 0},
	} {
		t.Run(tc.value, func(t *testing.T) {
			text := "[general]\n"
			if tc.value != "" {
				text += "upload_interval_ms = " + tc.value + "\n"
			}
			c, err := parse(text)
			if tc.want == 0 {
				if err == nil || !strings.Contains(err.Error(), "general.upload_interval_ms: expected an integer from 50 to 1000 milliseconds") {
					t.Fatalf("expected interval validation error, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if c.UploadIntervalMS != tc.want {
				t.Fatalf("upload interval: got %d, want %d", c.UploadIntervalMS, tc.want)
			}
		})
	}
}

func setup(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	return dir
}

func write(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustLoad(t *testing.T) *Config {
	t.Helper()
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestFirstRunWritesTheDefaultFile(t *testing.T) {
	setup(t)
	c := mustLoad(t)
	if data, err := os.ReadFile(Path()); err != nil || string(data) != DefaultTOML {
		t.Fatalf("default file not written: %v", err)
	}
	if c.MaxSeconds != 300 || c.TrimBelowDB != -45 || c.Tray.Hotkey != "<Super>space" {
		t.Fatalf("general: %+v", c.General)
	}
	if c.Split != (Split{Enabled: true, After: 10, Pause: 1}) {
		t.Fatalf("split: %+v", c.Split)
	}
	want := Conn{API: "openai", Endpoint: "https://api.openai.com/v1", AuthHeader: "Authorization", AuthScheme: "bearer", APIKey: "env:OPENAI_API_KEY", Model: "gpt-4o-mini-transcribe", Models: []string{"gpt-4o-mini-transcribe"}}
	if c.STT == nil || !reflect.DeepEqual(c.STT.Conn, want) || c.STT.Transform != "clean" {
		t.Fatalf("stt: %+v", c.STT)
	}
	if c.LLM == nil || c.LLM.Model != "gpt-4o-mini" || c.TransformModel("clean") != "gpt-4o-mini" || c.TransformModel("english") != "gpt-4o-mini" {
		t.Fatalf("llm: %+v", c.LLM)
	}
	if !reflect.DeepEqual(c.TransformOrder, []string{"clean", "punctuation", "english", "commit"}) {
		t.Fatal(c.TransformOrder)
	}
	if c.TTS == nil || c.TTS.Voice != "alloy" || c.TTS.Format != "wav" {
		t.Fatalf("tts: %+v", c.TTS)
	}
}

func TestAbsentSectionsAreNil(t *testing.T) {
	setup(t)
	write(t, Path(), "[general]\nmax_seconds = 30\n")
	c := mustLoad(t)
	if c.STT != nil || c.LLM != nil || c.TTS != nil || len(c.Transform) != 0 || c.MaxSeconds != 30 || c.PasteKeys != "auto" {
		t.Fatalf("%+v", c)
	}
}

func TestConnectionDefaults(t *testing.T) {
	setup(t)
	write(t, Path(), "[llm.providers.openai]\nendpoint = \"https://x/v1\"\napi_key = \"env:K\"\nmodel = \"m\"\n")
	c := mustLoad(t)
	want := Conn{API: "openai", Endpoint: "https://x/v1", AuthHeader: "Authorization", AuthScheme: "bearer", APIKey: "env:K", Model: "m"}
	if !reflect.DeepEqual(*c.LLM, want) {
		t.Fatalf("%+v", *c.LLM)
	}
}

func TestLiveConfiguration(t *testing.T) {
	const valid = "[live.providers.test]\nendpoint = \"wss://gateway.example/v1/realtime\"\nmodel = \"gemini.gemini-3.8-live\"\napi_key = \"cmd:pass show habla\"\n"
	for _, tc := range []struct{ text, problem string }{
		{valid, ""},
		{strings.ReplaceAll(valid, "cmd:pass show habla", "env:HABLA_API_KEY"), ""},
		{strings.ReplaceAll(valid, "cmd:pass show habla", "literal-secret"), "live.providers.test.api_key"},
		{strings.ReplaceAll(valid, "wss://", "https://"), "live.providers.test.endpoint"},
		{strings.ReplaceAll(valid, "gateway.example", "user:password@gateway.example"), "live.providers.test.endpoint"},
		{strings.ReplaceAll(valid, "gemini.gemini-3.8-live", ""), "live.providers.test.model"},
		{strings.ReplaceAll(valid, "model = \"gemini.gemini-3.8-live\"\n", ""), "live.providers.test.model"},
		{valid + "voices = 1\n", "live.providers.test.voices"},
	} {
		t.Run(tc.problem+tc.text[:6], func(t *testing.T) {
			setup(t)
			write(t, Path(), tc.text)
			c, err := Load()
			if tc.problem != "" {
				if err == nil || !strings.Contains(err.Error(), tc.problem) || strings.Contains(err.Error(), "literal-secret") || strings.Contains(err.Error(), "password") {
					t.Fatalf("unexpected validation error: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if c.Live == nil || c.Live.Model != "gemini.gemini-3.8-live" || c.STT != nil || c.TTS != nil {
				t.Fatal("live config missing or inherited another service")
			}
		})
	}
}

const sttConn = "[stt.providers.openai]\nendpoint = \"https://x/v1\"\napi_key = \"env:K\"\nmodel = \"m\"\n"

func TestLiveUsesSharedStyles(t *testing.T) {
	const live = "[live.providers.test]\nendpoint = \"wss://gateway/v1/realtime\"\nmodel = \"live-model\"\napi_key = \"env:KEY\"\nvoice = \"marin\"\n"
	const styles = "[styles.calm]\ninstructions = \" Speak with an Andalusian accent. \"\n"
	for _, tc := range []struct {
		extra, problem string
		active         bool
	}{
		{"style = \"calm\"\ninstructions_mode = \"inline\"\n", "", true},
		{"style = \"calm\"\ninstructions_mode = \"instructions\"\n", "", true},
		{"style = \"calm\"\n", "", false},
		{"instructions_mode = \"inline\"\n", "", true},
		{"style = \"missing\"\n", "live.providers.test.style", false},
		{"instructions_mode = \"system_instruction\"\n", "live.providers.test.instructions_mode", false},
	} {
		setup(t)
		write(t, Path(), live+tc.extra+styles)
		c, err := Load()
		if tc.problem != "" {
			if err == nil || !strings.Contains(err.Error(), tc.problem) {
				t.Fatalf("expected %s error, got %v", tc.problem, err)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		s := c.LiveDelivery()
		if tc.active {
			want := Delivery{Voice: "marin"}
			if strings.Contains(tc.extra, "style") {
				want.Instructions = "Speak with an Andalusian accent."
			}
			if s != want {
				t.Fatalf("shared style not resolved: %+v", s)
			}
		} else if s != (Delivery{}) {
			t.Fatal("inactive live style was applied")
		}
		if c.TTS != nil {
			t.Fatal("live configured a TTS")
		}
	}
}

func TestInvalidFileIsAnErrorNamingTheLineAndIsUntouched(t *testing.T) {
	for _, tc := range []struct{ text, want string }{
		{"[general]\nmax_seconds = \"x\"\n", `line 2 (last key "general.max_seconds")`},
		{"[general]\npaste_keys = \"alt+v\"\n", "general.paste_keys: expected auto | ctrl+v | ctrl+shift+v (at line 2)"},
		{"[general]\nlog_level = \"loud\"\n", "general.log_level: expected error | warn | info | debug (at line 2)"},
		{"max_seconds = 30\n", "unknown key max_seconds (at line 1)"},
		{"[stt.providers.openai]\napi_key = \"env:K\"\nmodel = \"m\"\n", "stt.providers.openai.endpoint: required"},
		{"[stt.providers.openai]\nendpoint = \"https://x\"\nmodel = \"m\"\n", "stt.providers.openai.api_key: required"},
		{"[stt.providers.openai]\nendpoint = \"https://x\"\napi_key = \"env:K\"\n", "stt.providers.openai.model: required"},
		{sttConn + "api = \"azure\"\n", "stt.providers.openai.api: expected openai | gemini | elevenlabs (at line 5)"},
		{sttConn + "auth_scheme = \"basic\"\n", "stt.providers.openai.auth_scheme: expected bearer | raw (at line 5)"},
		{"[stt.providers.openai]\nendpoint = \"https://x\"\napi_key = \"sk-literal\"\nmodel = \"m\"\n", "stt.providers.openai.api_key: expected env:VARIABLE or cmd:COMMAND (at line 3)"},
		{sttConn + "transform = \"nope\"\n", "stt.providers.openai.transform: expected a name in [stt.transforms.*] (at line 5)"},
		{"[stt.transforms.x]\nmodel = \"m\"\n", "stt.transforms.x.prompt: required"},
		{"[tts.providers.openai]\nendpoint = \"https://x\"\napi_key = \"env:K\"\nmodel = \"m\"\n", "tts.providers.openai.voice: required"},
		{"[tts.providers.openai]\nendpoint = \"https://x\"\napi_key = \"env:K\"\nmodel = \"m\"\nvoice = \"Kore\"\napi = \"gemini\"\nformat = \"mp3\"\n", "tts.providers.openai.format: expected wav with api = \"gemini\""},
		{"[tts.providers.openai]\nendpoint = \"https://x\"\napi_key = \"env:K\"\nmodel = \"m\"\nvoice = \"Kore\"\napi = \"gemini\"\ninstructions_mode = \"instructions\"\n", "tts.providers.openai.instructions_mode: instructions needs api = \"openai\""},
		{ttsConfigTextForTest + "style = \"nope\"\n", "tts.providers.openai.style: expected a name in [styles.*] (at line 6)"},
		{ttsConfigTextForTest + "instructions_mode = \"ssml\"\n", "tts.providers.openai.instructions_mode: expected inline | instructions | system_instruction (at line 6)"},
		{ttsConfigTextForTest + "instructions_mode = \"system_instruction\"\n", "tts.providers.openai.instructions_mode: system_instruction needs api = \"gemini\" (at line 6)"},
		{"[tts.providers.openai]\nendpoint = \"https://x\"\napi_key = \"env:K\"\nmodel = \"m\"\nvoice = \"Kore\"\naccent_prompt = \"x\"\n", "unknown key tts.providers.openai.accent_prompt"},
		{sttConn + "api = \"elevenlabs\"\n", "stt.providers.openai.api: elevenlabs supports tts only (at line 5)"},
		{"[llm.providers.x]\nendpoint = \"https://x\"\napi_key = \"env:K\"\nmodel = \"m\"\napi = \"elevenlabs\"\n", "llm.providers.x.api: elevenlabs supports tts only"},
		{elevenText + "instructions_mode = \"inline\"\n", "tts.providers.e.instructions_mode: elevenlabs has no instructions; pick an accent with the voice (at line 10)"},
		{elevenText + "format = \"mp3\"\n", "tts.providers.e.format: expected wav with api = \"elevenlabs\""},
		{"[split]\nenabled = true\nafter = 0\n", "split.after: expected a finite number of seconds > 0 (at line 3)"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			setup(t)
			write(t, Path(), tc.text)
			c, err := Load()
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), Path()) {
				t.Fatalf("error %v, want %q", err, tc.want)
			}
			if c != nil {
				t.Fatalf("config with error: %+v", c)
			}
			if data, _ := os.ReadFile(Path()); string(data) != tc.text {
				t.Fatal("invalid file was changed")
			}
		})
	}
}

const ttsConfigTextForTest = "[tts.providers.openai]\nendpoint = \"https://x/v1\"\napi_key = \"env:K\"\nmodel = \"m\"\nvoice = \"Kore\"\n"

func TestFileIsReread(t *testing.T) {
	setup(t)
	mustLoad(t)
	write(t, Path(), "[general]\nmax_seconds = 40\n")
	c := mustLoad(t)
	if c.MaxSeconds != 40 || c.STT != nil {
		t.Fatalf("not reread: %+v", c)
	}
}

func TestInvalidUTF8AndFilesystemError(t *testing.T) {
	setup(t)
	write(t, Path(), "language = \"\xff\"\n")
	if c, err := Load(); err == nil || c != nil {
		t.Fatalf("%v %+v", err, c)
	}
	dir := setup(t)
	write(t, filepath.Join(dir, "file"), "")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "file"))
	if c, err := Load(); err == nil || c != nil {
		t.Fatalf("%v %+v", err, c)
	}
}

func TestSaveChoiceEditsConfigInPlace(t *testing.T) {
	setup(t)
	mustLoad(t)
	for k, v := range map[string]any{"transform": "punctuation", "language": "en", "stt_model": "gpt-4o-mini-transcribe", "llm_model": "gpt-4o", "tts_model": "t", "paste": true, "paste_enter": true} {
		if err := SaveChoice(k, v); err != nil {
			t.Fatal(err)
		}
	}
	c := mustLoad(t)
	if c.STT.Transform != "punctuation" || c.STT.Language != "en" || !c.Tray.Paste || !c.Tray.PasteEnter || c.LLM.Model != "gpt-4o" || c.TTS.Model != "t" {
		t.Fatalf("%+v %+v", c.STT, c.General)
	}
	data, _ := os.ReadFile(Path())
	want := strings.NewReplacer(`transform     = "clean" `, `transform     = "punctuation" `,
		`language    = "" `, `language    = "en" `, "paste         = false", "paste         = true", "paste_enter   = false", "paste_enter   = true",
		`model       = "gpt-4o-mini"`, `model       = "gpt-4o"`, `model              = "gpt-4o-mini-tts"`, `model              = "t"`).Replace(DefaultTOML)
	if string(data) != want {
		t.Fatalf("config.toml:\n%s", data)
	}
}

func TestTrayKeysMovedOutOfGeneral(t *testing.T) {
	for _, key := range []string{"hotkey", "paste", "paste_enter"} {
		if _, err := parse("[general]\n" + key + " = true\n"); err == nil || !strings.HasSuffix(err.Error(), ": moved to tray."+key) {
			t.Errorf("%s: %v", key, err)
		}
	}
}

func TestSaveChoiceAddsMissingKeyAndSection(t *testing.T) {
	setup(t)
	write(t, Path(), "[stt.providers.openai] # dictation\nendpoint = \"https://x/v1\"\napi_key = \"env:K\"\nmodel = 'm'\n")
	if err := SaveChoice("language", "es"); err != nil {
		t.Fatal(err)
	}
	if err := SaveChoice("paste", true); err != nil {
		t.Fatal(err)
	}
	if err := SaveChoice("stt_model", "n"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(Path())
	want := "[stt.providers.openai] # dictation\nlanguage = \"es\"\nendpoint = \"https://x/v1\"\napi_key = \"env:K\"\nmodel = \"n\"\n\n[tray]\npaste = true\n"
	if string(data) != want {
		t.Fatalf("config.toml:\n%s", data)
	}
}

func TestSaveChoiceRefusesAnInvalidResult(t *testing.T) {
	setup(t)
	mustLoad(t)
	if err := SaveChoice("transform", "gone"); err == nil {
		t.Fatal("unknown transform saved")
	}
	if data, _ := os.ReadFile(Path()); string(data) != DefaultTOML {
		t.Fatalf("file changed:\n%s", data)
	}
}

func TestSaveChoiceKeepsASymlink(t *testing.T) {
	dir := setup(t)
	real := filepath.Join(dir, "dotfiles", "config.toml")
	write(t, real, DefaultTOML)
	if err := os.MkdirAll(filepath.Dir(Path()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, Path()); err != nil {
		t.Fatal(err)
	}
	if err := SaveChoice("transform", ""); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(Path()); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("link replaced: %v", err)
	}
	if c := mustLoad(t); c.STT.Transform != "" {
		t.Fatalf("transform = %q", c.STT.Transform)
	}
}

func TestSplitValidation(t *testing.T) {
	for _, tc := range []struct {
		text string
		want Split
	}{
		{"", Split{true, 10, 1}},
		{"[general]\nmax_seconds = 1\n", Split{true, 10, 1}},
		{"[general]\nmax_seconds = 5\n", Split{true, 10, 1}},
		{"[split]\nenabled = false\n", Split{false, 10, 1}},
		{"[split]\nafter = 5\npause = 1.5\n", Split{true, 5, 1.5}},
		{"[general]\nmax_seconds = 5\n[split]\nafter = 20\npause = 20\n", Split{true, 20, 20}},
		{"[split]\nafter = 1e300\npause = 1e300\n", Split{true, 1e300, 1e300}},
	} {
		c, err := parse(tc.text)
		if err != nil {
			t.Errorf("%q: %v", tc.text, err)
		} else if c.Split != tc.want {
			t.Errorf("%q: split = %+v, want %+v", tc.text, c.Split, tc.want)
		}
	}
	for _, field := range []string{"after", "pause"} {
		for _, value := range []string{"0", "-1", "nan", "inf", "-inf"} {
			text := "[split]\nenabled = true\n" + field + " = " + value + "\n"
			c, err := parse(text)
			if err == nil || c != nil || !strings.Contains(err.Error(), "split."+field+": expected a finite number of seconds > 0") {
				t.Errorf("%q: %+v %v", text, c, err)
			}
		}
	}
}

func TestDelivery(t *testing.T) {
	ttsConfigText := "[tts.providers.openai]\nendpoint = \"https://x/v1\"\napi_key = \"env:K\"\nmodel = \"m\"\nvoice = \"Kore\"\n"
	styles := "[styles.sevilla]\ninstructions = \" Andaluz. \"\n"
	for _, tc := range []struct {
		extra, voice, instructions string
		warns                      bool
	}{
		{"", "Kore", "", false},
		{"instructions_mode = \"inline\"\n", "Kore", "", false},
		{"instructions_mode = \"inline\"\nstyle = \"sevilla\"\n", "Kore", "Andaluz.", false},
		{"style = \"sevilla\"\n", "Kore", "", true},
		{"style = \"\"\n", "Kore", "", false},
	} {
		setup(t)
		write(t, Path(), ttsConfigText+tc.extra+styles)
		voice, instructions, warning := mustLoad(t).TTS.Delivery()
		if voice != tc.voice || instructions != tc.instructions || (warning != "") != tc.warns {
			t.Errorf("%q: %q %q %q", tc.extra, voice, instructions, warning)
		}
	}
}

func TestPrices(t *testing.T) {
	c, err := parse("[prices]\n\"m\" = { input = 1.5, audio = 3, output = 6 }\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Prices["m"]; got != (Price{Input: 1.5, Audio: 3, Output: 6}) {
		t.Fatalf("%+v", got)
	}
	if _, err := parse("[prices]\n\"m\" = { inptu = 1 }\n"); err == nil || !strings.Contains(err.Error(), "unknown key") {
		t.Fatalf("want an unknown key error, got %v", err)
	}
}

const twoProviders = `[stt]
provider = "work"

[stt.providers.work]
endpoint = "https://a/v1"
api_key  = "env:A"
model    = "big"
language = "es"
transform  = "fix"

[stt.providers.english]
endpoint = "https://b/v1"
api_key  = "env:B"
model    = "small"
language = "en"

[stt.transforms.fix]
prompt = """
Fix punctuation.
[not a header]
model = "not a key"
"""
`

func TestProviders(t *testing.T) {
	c, err := parse(twoProviders)
	if err != nil {
		t.Fatal(err)
	}
	if c.STT.Model != "big" || c.STT.Language != "es" || c.STT.API != "openai" || c.STT.AuthScheme != "bearer" {
		t.Fatalf("active stt: %+v", c.STT)
	}
	if want := (Providers{Active: "work", Names: []string{"work", "english"}}); !reflect.DeepEqual(c.Providers["stt"], want) {
		t.Fatalf("providers: %+v", c.Providers["stt"])
	}
	if got := c.Transform["fix"].Prompt; !strings.Contains(got, "[not a header]\nmodel = \"not a key\"") {
		t.Fatalf("multi-line prompt: %q", got)
	}
	if err := c.Use("stt", "english"); err != nil || c.STT.Model != "small" || c.STT.Language != "en" || c.Providers["stt"].Active != "english" {
		t.Fatalf("Use: %v %+v", err, c.STT)
	}
	if err := c.Use("stt", "nope"); err == nil || !strings.Contains(err.Error(), "expected one of work | english") {
		t.Fatalf("unknown provider: %v", err)
	}
	if err := c.Use("tts", "x"); err == nil || !strings.Contains(err.Error(), "no [tts]") {
		t.Fatalf("absent service: %v", err)
	}
}

func TestUseStyleOverridesTheProvidersStyle(t *testing.T) {
	setup(t)
	write(t, Path(), DefaultTOML)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := c.UseStyle("tts", "sevilla"); err != nil || c.TTS.Style != "sevilla" || c.TTS.Instructions != strings.TrimSpace(c.Styles["sevilla"].Instructions) {
		t.Fatalf("sevilla: %v %+v", err, c.TTS)
	}
	if err := c.UseStyle("live", ""); err != nil || c.Live.Style != "" || c.Live.Instructions != "" {
		t.Fatalf("no style: %v %+v", err, c.Live)
	}
	if err := c.UseStyle("tts", "nope"); err == nil || !strings.Contains(err.Error(), "| storyteller, or") {
		t.Fatalf("unknown style: %v", err)
	}
}

func TestActiveProvider(t *testing.T) {
	one := "[stt.providers.solo]\nendpoint = \"https://a/v1\"\napi_key = \"env:A\"\nmodel = \"m\"\n"
	if c, err := parse(one); err != nil || c.Providers["stt"].Active != "solo" || c.STT.Model != "m" {
		t.Fatalf("single provider: %v %+v", err, c)
	}
	for text, want := range map[string]string{
		strings.Replace(twoProviders, "provider = \"work\"\n", "", 1):                 "stt.provider: required to choose between work | english",
		strings.Replace(twoProviders, `provider = "work"`, `provider = "home"`, 1):    "stt.provider: expected one of work | english",
		"[stt]\nprovider = \"x\"\n":                                                   "stt: no [stt.providers.<name>]",
		strings.Replace(one, "solo", `"with space"`, 1):                               "a name uses only letters, digits, - and _",
		strings.Replace(twoProviders, `transform  = "fix"`, `transform  = "gone"`, 1): "stt.providers.work.transform: expected a name in [stt.transforms.*]",
	} {
		if _, err := parse(text); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v, want %q", text, err, want)
		}
	}
}

func TestStyles(t *testing.T) {
	base := "[tts.providers.a]\nendpoint = \"https://a/v1\"\napi_key = \"env:A\"\nmodel = \"m\"\nvoice = \"alloy\"\ninstructions_mode = \"instructions\"\nstyle = \"calm\"\n"
	c, err := parse(base + "[styles.calm]\ninstructions = \"\"\"\n  Speak calmly.\n\"\"\"\n")
	if err != nil {
		t.Fatal(err)
	}
	if voice, instructions, _ := c.TTS.Delivery(); voice != "alloy" || instructions != "Speak calmly." {
		t.Fatalf("delivery: %q %q", voice, instructions)
	}
	if _, err := parse(base); err == nil || !strings.Contains(err.Error(), "tts.providers.a.style: expected a name in [styles.*]") {
		t.Fatalf("missing style: %v", err)
	}
	if _, err := parse(base + "[styles.calm]\nvoice = \"nova\"\ninstructions = \"x\"\n"); err == nil || !strings.Contains(err.Error(), "unknown key") {
		t.Fatalf("style voice: %v", err)
	}
}

func TestSetKeySkipsMultilineStrings(t *testing.T) {
	text := "[stt.transforms.fix]\nprompt = \"\"\"\n[stt.providers.work]\nmodel = \"inside\"\n\"\"\"\n\n[stt.providers.work]\nmodel = \"big\"\n"
	got := setKey(text, "stt.providers.work", "model", `"small"`)
	if !strings.Contains(got, "model = \"inside\"") || !strings.Contains(got, "[stt.providers.work]\nmodel = \"small\"") {
		t.Fatalf("setKey edited the prompt or missed the key:\n%s", got)
	}
	if line := lineOf(text, "stt.providers.work", "model"); line != 8 {
		t.Fatalf("lineOf = %d, want 8", line)
	}
}

func TestSaveChoiceTargetsTheActiveProvider(t *testing.T) {
	setup(t)
	write(t, Path(), twoProviders)
	if err := SaveChoice("stt_model", "huge"); err != nil {
		t.Fatal(err)
	}
	if c := mustLoad(t); c.STT.Model != "huge" || c.file.STT.Providers["english"].Model != "small" {
		t.Fatalf("model saved to the wrong provider: %+v", c.file.STT.Providers)
	}
	if err := SaveChoice("stt_provider", "english"); err != nil {
		t.Fatal(err)
	}
	if c := mustLoad(t); c.Providers["stt"].Active != "english" || c.STT.Model != "small" {
		t.Fatalf("provider not switched: %+v", c.Providers["stt"])
	}
	if err := SaveChoice("language", "fr"); err != nil {
		t.Fatal(err)
	}
	if c := mustLoad(t); c.STT.Language != "fr" || c.file.STT.Providers["work"].Language != "es" {
		t.Fatalf("language saved to the wrong provider")
	}
	data, _ := os.ReadFile(Path())
	if !strings.Contains(string(data), "[not a header]\nmodel = \"not a key\"") {
		t.Fatalf("the prompt changed:\n%s", data)
	}
}

const elevenText = "[tts.providers.e]\napi = \"elevenlabs\"\nendpoint = \"https://x/v1\"\nauth_header = \"xi-api-key\"\nauth_scheme = \"raw\"\napi_key = \"env:K\"\nmodel = \"m\"\nvoice = \"George\"\nvoices = [\"George=JB\", \"plain\"]\n"

func TestElevenLabsStyleWarning(t *testing.T) {
	setup(t)
	write(t, Path(), elevenText+"style = \"s\"\n[styles.s]\ninstructions = \"x\"\n")
	if _, _, w := mustLoad(t).TTS.Delivery(); w != "elevenlabs has no styles; pick an accent with the voice" {
		t.Fatal(w)
	}
}

func TestElevenLabsTTSIsAccepted(t *testing.T) {
	setup(t)
	write(t, Path(), elevenText+"format = \"wav\"\n")
	if c := mustLoad(t); c.TTS.API != "elevenlabs" {
		t.Fatalf("%+v", c.TTS)
	}
}

func TestVoiceEntry(t *testing.T) {
	for in, want := range map[string][2]string{"JB": {"", "JB"}, "George=JB": {"George", "JB"}, "a=b=c": {"a", "b=c"}, "": {"", ""}} {
		if name, id := VoiceEntry(in); name != want[0] || id != want[1] {
			t.Errorf("%q: %q %q", in, name, id)
		}
	}
}

func TestDeliveryMapsVoiceNamesToIDs(t *testing.T) {
	for voice, want := range map[string]string{"George": "JB", "plain": "plain", "JB": "JB", "other": "other"} {
		setup(t)
		write(t, Path(), strings.Replace(elevenText, `voice = "George"`, `voice = "`+voice+`"`, 1))
		if got, _, _ := mustLoad(t).TTS.Delivery(); got != want {
			t.Errorf("%s: %q", voice, got)
		}
	}
	setup(t)
	write(t, Path(), "[live.providers.l]\nendpoint = \"wss://x/v1/realtime\"\nmodel = \"m\"\napi_key = \"env:K\"\nvoice = \"Ana\"\nvoices = [\"Ana=A1\"]\ninstructions_mode = \"inline\"\n")
	if got := mustLoad(t).LiveDelivery().Voice; got != "A1" {
		t.Errorf("live voice %q", got)
	}
}

func TestDefaultsIncludeInactiveElevenLabs(t *testing.T) {
	c := Defaults()
	if c.TTS.API == "elevenlabs" || !slices.Contains(c.Providers["tts"].Names, "elevenlabs") {
		t.Fatalf("%+v", c.Providers["tts"])
	}
}
