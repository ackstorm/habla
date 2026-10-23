// Package config loads config.toml, which belongs to the user; the tray menu writes its choices into it.
package config

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/BurntSushi/toml"
)

// DefaultTOML is written on first run, never afterwards.
const DefaultTOML = `# Getting started: export OPENAI_API_KEY (or set the stt, llm and tts providers to "gemini" and export GEMINI_API_KEY).
# The tray menu or --provider NAME switch providers; the inactive providers below are alternatives.
# The hotkey is hold-to-talk, and every service is optional.
[general]
max_seconds   = 300
trim_below_db = -45                  # cut quieter audio (dBFS) at both ends; all quieter = not sent; 0 = off
upload_interval_ms = 50              # audio upload polling interval while recording; 50–1000 ms
paste_keys    = "auto"               # auto | ctrl+v | ctrl+shift+v
log_level     = "warn"               # error | warn | info | debug, in ~/.local/state/habla/habla.log

# habla tray only; habla listen takes --paste / --paste_enter instead.
[tray]
hotkey        = "<Super>space"
paste         = false                # true = also paste into the focused window
paste_enter   = false                # true = and press Enter after pasting (needs paste)

# Each service is optional: without [llm] no transform, without [stt] no dictation, without [tts] no habla say,
# without [live] no habla live. A service has one or more [<service>.providers.<name>] (names: letters, digits, -
# and _); "provider" picks the active one: the tray switches it, --provider NAME overrides it for one run. With a
# single provider, "provider" may be left out.
[llm]
provider = "openai"

[llm.providers.openai]
api         = "openai"               # openai | gemini
endpoint    = "https://api.openai.com/v1"
auth_header = "Authorization"
auth_scheme = "bearer"
api_key     = "env:OPENAI_API_KEY"
model       = "gpt-4o-mini"
models      = ["gpt-4o-mini"]

[llm.providers.gemini]
api         = "gemini"
endpoint    = "https://generativelanguage.googleapis.com/v1beta"
auth_header = "x-goog-api-key"
auth_scheme = "raw"
api_key     = "env:GEMINI_API_KEY"
model       = "gemini-3.8-flash"
models      = ["gemini-3.8-flash"]

[stt]
provider = "openai"

[stt.providers.openai]
api         = "openai"
endpoint    = "https://api.openai.com/v1"
auth_header = "Authorization"
auth_scheme = "bearer"
api_key     = "env:OPENAI_API_KEY"
model       = "gpt-4o-mini-transcribe"
models      = ["gpt-4o-mini-transcribe"]
language    = ""                     # "" = auto
languages   = ["en"]
transform     = "clean"              # a [stt.transforms.*] name; unset: no transform applied

[stt.providers.gemini]
api         = "gemini"
endpoint    = "https://generativelanguage.googleapis.com/v1beta"
auth_header = "x-goog-api-key"
auth_scheme = "raw"
api_key     = "env:GEMINI_API_KEY"
model       = "gemini-3.5-transcribe"
models      = ["gemini-3.5-transcribe"]
language    = ""
languages   = ["en"]
transform     = "clean"

# Transform prompts; one may span lines: prompt = """ … """
[stt.transforms.clean]
# Vocabulary and fixed spellings go in the prompt too, e.g. "Always write Kubernetes and PostgreSQL."
prompt = "Clean up this dictation: remove filler words (um, uh, like, you know) and false starts, and when the speaker corrects themselves keep only the final version. Fix punctuation and capitalization. Do not change the meaning or add anything. If unsure, leave the text as it is."

[stt.transforms.punctuation]
prompt = "Fix punctuation and casing. Do not change any words."

[stt.transforms.english]
prompt = "Translate to English."

[stt.transforms.commit]
prompt = "Rewrite as a one-line Angular commit message: type(scope): subject."

[tts]
provider = "openai"

[tts.providers.openai]
api                = "openai"
endpoint           = "https://api.openai.com/v1"
auth_header        = "Authorization"
auth_scheme        = "bearer"
api_key            = "env:OPENAI_API_KEY"
model              = "gpt-4o-mini-tts"
models             = ["gpt-4o-mini-tts"]
voice              = "alloy"
voices             = ["alloy", "ash", "coral", "nova", "sage", "marin", "cedar"]
format             = "wav"
# instructions_mode = "instructions" # unset: styles not applied; inline | instructions
# style = "storyteller"              # a [styles.*] name; unset: no style applied

[tts.providers.gemini]
api                = "gemini"
endpoint           = "https://generativelanguage.googleapis.com/v1beta"
auth_header        = "x-goog-api-key"
auth_scheme        = "raw"
api_key            = "env:GEMINI_API_KEY"
model              = "gemini-3.8-flash-tts"
models             = ["gemini-3.8-flash-tts"]
voice              = "Kore"
voices             = ["Kore", "Puck", "Charon", "Aoede"]
format             = "wav"
# instructions_mode = "system_instruction" # inline | system_instruction
# style = "storyteller"

[tts.providers.elevenlabs]
api         = "elevenlabs"
endpoint    = "https://api.elevenlabs.io/v1"
auth_header = "xi-api-key"
auth_scheme = "raw"
api_key     = "env:ELEVENLABS_API_KEY"
model       = "eleven_multilingual_v2"
models      = ["eleven_multilingual_v2", "eleven_flash_v2_5", "eleven_v3"]
voice       = "George"
voices      = ["George=JBFqnCBsd6RMkjVDRZzb", "Alice=Xb7hH8MSUJpSbSDYk0k2", "Daniel=onwK4e9ZLuTAKqWW03F9"]
format      = "wav"

[live]
provider = "openai"

[live.providers.openai]
endpoint = "wss://api.openai.com/v1/realtime"
api_key  = "env:OPENAI_API_KEY"
model    = "gpt-realtime"
voice    = "marin"
voices   = ["marin", "cedar"]
instructions_mode = "instructions" # inline | instructions; unset: voice and style not applied
# style = "storyteller"

# Delivery styles shared by tts and live providers; instructions may span lines: instructions = """ … """
[styles.default]
instructions = "Speak clearly in a calm, friendly tone."

[styles.storyteller]
instructions = "Read like a warm storyteller, with gentle pauses."

[styles.sevilla]
instructions = "Speak Spanish with an Andalusian accent from Seville."

[styles.english]
instructions = "Speak English with a neutral British accent."

[split]
enabled = true
after   = 10                         # seconds a piece must reach before a pause can cut it
pause   = 1                          # seconds of silence that cut it

# USD per million tokens, for habla stats when the gateway sends no x-litellm-response-cost header.
# [prices]
# "gpt-4o-mini-transcribe" = { input = 1.25, audio = 3.0, output = 5.0 }   # example values: check your provider
`

// PasteKeys are the valid paste_keys values.
var PasteKeys = []string{"auto", "ctrl+v", "ctrl+shift+v"}

// LogLevels are the valid log_level (and --log) values.
var LogLevels = []string{"error", "warn", "info", "debug"}

var (
	APIs        = []string{"openai", "gemini", "elevenlabs"}
	AuthSchemes = []string{"bearer", "raw"}
)

// services are the sections made of providers.
var services = []string{"llm", "stt", "tts", "live"}

// Names are bare TOML keys, so every header can be written unquoted.
func validName(name string) bool {
	return name != "" && strings.Trim(name, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_-") == ""
}

// Config is config.toml with each service resolved to its active provider (nil: the service is not configured).
type Config struct {
	General
	Tray           Tray
	Paste          bool // habla listen --paste (or --paste_enter)
	PasteEnter     bool // habla listen --paste_enter
	STT            *STT
	LLM            *Conn
	TTS            *TTS
	Live           *Live
	Transform      map[string]Transform // [stt.transforms.*]
	TransformOrder []string             // their names in file order
	Styles         map[string]Style     // [styles.*]
	Split          Split
	Prices         map[string]Price
	Providers      map[string]Providers // per configured service ("stt", "llm", "tts", "live")
	file           *file                // every provider, for Use
}

// Providers are one service's provider names in file order and the active one.
type Providers struct {
	Active string
	Names  []string
}

// file is config.toml as written.
type file struct {
	General `toml:"general"`
	Tray    Tray               `toml:"tray"`
	STT     *sttFile           `toml:"stt"`
	LLM     *serviceFile[Conn] `toml:"llm"`
	TTS     *serviceFile[TTS]  `toml:"tts"`
	Live    *serviceFile[Live] `toml:"live"`
	Styles  map[string]Style   `toml:"styles"`
	Split   Split              `toml:"split"`
	Prices  map[string]Price   `toml:"prices"`
}

type serviceFile[T any] struct {
	Provider  string       `toml:"provider"`
	Providers map[string]T `toml:"providers"`
}

type sttFile struct {
	Provider   string               `toml:"provider"`
	Providers  map[string]STT       `toml:"providers"`
	Transforms map[string]Transform `toml:"transforms"`
}

type Tray struct {
	Hotkey     string `toml:"hotkey"`
	Paste      bool   `toml:"paste"`
	PasteEnter bool   `toml:"paste_enter"`
}

type General struct {
	MaxSeconds       int    `toml:"max_seconds"`
	TrimBelowDB      int    `toml:"trim_below_db"`
	UploadIntervalMS int    `toml:"upload_interval_ms"`
	PasteKeys        string `toml:"paste_keys"`
	LogLevel         string `toml:"log_level"`
}
type Conn struct {
	API        string   `toml:"api"`
	Endpoint   string   `toml:"endpoint"`
	AuthHeader string   `toml:"auth_header"`
	AuthScheme string   `toml:"auth_scheme"`
	APIKey     string   `toml:"api_key"`
	Model      string   `toml:"model"`
	Models     []string `toml:"models"` // tray menu choices for model
}

// Live is the single realtime WebSocket protocol; credentials use the same sources as Conn.
type Live struct {
	Endpoint         string   `toml:"endpoint"`
	Model            string   `toml:"model"`
	APIKey           string   `toml:"api_key"`
	Voice            string   `toml:"voice"`
	Voices           []string `toml:"voices"` // tray menu choices for voice
	InstructionsMode string   `toml:"instructions_mode"`
	Style            string   `toml:"style"`
	Instructions     string   `toml:"-"` // the style's, resolved
}
type STT struct {
	Conn
	Language  string   `toml:"language"`
	Languages []string `toml:"languages"`
	Transform string   `toml:"transform"`
}
type Transform struct {
	Model  string `toml:"model"`
	Prompt string `toml:"prompt"`
}

type Split struct {
	Enabled bool    `toml:"enabled"`
	After   float64 `toml:"after"` // seconds
	Pause   float64 `toml:"pause"` // seconds
}

type TTS struct {
	Conn
	Voice            string   `toml:"voice"`
	Voices           []string `toml:"voices"` // tray menu choices for voice
	Format           string   `toml:"format"`
	InstructionsMode string   `toml:"instructions_mode"`
	Style            string   `toml:"style"`
	Instructions     string   `toml:"-"` // the style's, resolved
}

// Style is one [styles.*] entry, shared by tts and live providers. The voice always comes from the provider.
type Style struct {
	Instructions string `toml:"instructions"`
}

// Delivery is the voice and style instructions a live session applies.
type Delivery struct {
	Voice, Instructions string
}

// Price is USD per million tokens of one model, for habla stats when the reply has no gateway cost.
type Price struct {
	Input  float64 `toml:"input"`
	Audio  float64 `toml:"audio"`
	Output float64 `toml:"output"`
}

var InstructionModes = []string{"inline", "instructions", "system_instruction"}

// Path is $XDG_CONFIG_HOME/habla/config.toml.
func Path() string { return filepath.Join(xdg("XDG_CONFIG_HOME", ".config"), "habla", "config.toml") }

// StateDir is $XDG_STATE_HOME/habla.
func StateDir() string {
	return filepath.Join(xdg("XDG_STATE_HOME", filepath.Join(".local", "state")), "habla")
}

func xdg(variable, fallback string) string {
	if dir := os.Getenv(variable); dir != "" {
		return dir
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, fallback)
}

// Defaults returns the built-in configuration: DefaultTOML, decoded.
func Defaults() *Config {
	c, err := decode(DefaultTOML, &file{})
	if err != nil {
		panic(err)
	}
	return c
}

// Load reads config.toml. It writes the default file on first run,
// never afterwards, and re-reads the file on every call. An invalid file gives no config and an error naming the
// line; the file is left untouched.
func Load() (*Config, error) {
	path := Path()
	c, err := load(path)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

func load(path string) (*Config, error) {
	if err := writeDefault(path); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if !utf8.Valid(data) {
		return nil, errors.New("not valid UTF-8")
	}
	return parse(string(data))
}

// writeDefault creates path with DefaultTOML unless it exists. A hard link never replaces a file created meanwhile,
// and readers never see it half written.
func writeDefault(path string) error {
	if _, err := os.Stat(path); err == nil || !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*.toml")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(DefaultTOML); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Link(tmp.Name(), path); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	return nil
}

func parse(text string) (*Config, error) {
	d := Defaults()
	return decode(text, &file{General: d.General, Tray: d.Tray, Split: d.Split})
}

func decode(text string, f *file) (*Config, error) {
	md, err := toml.Decode(text, f)
	if err != nil {
		return nil, err // BurntSushi/toml names the line: `toml: line 2 (last key "max_seconds"): …`
	}
	if keys := md.Undecoded(); len(keys) > 0 {
		k := keys[0]
		moved := ""
		if len(k) == 2 && k[0] == "general" && slices.Contains([]string{"hotkey", "paste", "paste_enter"}, k[1]) {
			moved = ": moved to tray." + k[1]
		}
		return nil, errors.New("unknown key " + k.String() + at(lineOf(text, strings.Join(k[:len(k)-1], "."), k[len(k)-1])) + moved)
	}
	order := map[string][]string{} // "stt.providers", "stt.transforms", "styles", … → names in file order
	for _, k := range md.Keys() {
		if len(k) == 3 && slices.Contains(services, k[0]) && (k[1] == "providers" || k[1] == "transforms") ||
			len(k) == 2 && k[0] == "styles" {
			if !validName(k[len(k)-1]) {
				return nil, fmt.Errorf("%s: a name uses only letters, digits, - and _", k)
			}
			table := strings.Join(k[:len(k)-1], ".")
			order[table] = append(order[table], k[len(k)-1])
		}
	}
	c := &Config{General: f.General, Tray: f.Tray, Styles: f.Styles, Split: f.Split, Prices: f.Prices, Providers: map[string]Providers{}, file: f}
	for _, name := range order["styles"] {
		if strings.TrimSpace(f.Styles[name].Instructions) == "" {
			return nil, fmt.Errorf("styles.%s.instructions: required", name)
		}
	}
	style := func(section, name string) error {
		if _, ok := f.Styles[name]; name != "" && !ok {
			return invalid(text, section, "style", "a name in [styles.*]")
		}
		return nil
	}
	if s := f.STT; s != nil {
		c.Transform, c.TransformOrder = s.Transforms, order["stt.transforms"]
		for _, name := range c.TransformOrder {
			if strings.TrimSpace(s.Transforms[name].Prompt) == "" {
				return nil, fmt.Errorf("stt.transforms.%s.prompt: required", name)
			}
		}
		for _, name := range order["stt.providers"] {
			p, section := s.Providers[name], "stt.providers."+name
			if err := checkConn(text, md, section, &p.Conn); err != nil {
				return nil, err
			}
			if _, ok := s.Transforms[p.Transform]; p.Transform != "" && !ok {
				return nil, invalid(text, section, "transform", "a name in [stt.transforms.*]")
			}
			s.Providers[name] = p
		}
	}
	if l := f.LLM; l != nil {
		for _, name := range order["llm.providers"] {
			p := l.Providers[name]
			if err := checkConn(text, md, "llm.providers."+name, &p); err != nil {
				return nil, err
			}
			l.Providers[name] = p
		}
	}
	if t := f.TTS; t != nil {
		for _, name := range order["tts.providers"] {
			p, section := t.Providers[name], "tts.providers."+name
			if err := checkConn(text, md, section, &p.Conn); err != nil {
				return nil, err
			}
			if !md.IsDefined("tts", "providers", name, "voice") {
				return nil, fmt.Errorf("%s.voice: required", section)
			}
			if (p.API == "gemini" || p.API == "elevenlabs") && p.Format != "" && p.Format != "wav" {
				return nil, invalid(text, section, "format", `wav with api = "`+p.API+`"`)
			}
			if p.API == "elevenlabs" && p.InstructionsMode != "" {
				return nil, fmt.Errorf("%s.instructions_mode: elevenlabs has no instructions; pick an accent with the voice%s", section, at(lineOf(text, section, "instructions_mode")))
			}
			if p.InstructionsMode != "" && !slices.Contains(InstructionModes, p.InstructionsMode) {
				return nil, invalid(text, section, "instructions_mode", strings.Join(InstructionModes, " | "))
			}
			if (p.InstructionsMode == "system_instruction" && p.API != "gemini") ||
				(p.InstructionsMode == "instructions" && p.API == "gemini") {
				want := map[string]string{"system_instruction": `api = "gemini"`, "instructions": `api = "openai"`}[p.InstructionsMode]
				return nil, fmt.Errorf("%s.instructions_mode: %s needs %s%s", section, p.InstructionsMode, want, at(lineOf(text, section, "instructions_mode")))
			}
			if err := style(section, p.Style); err != nil {
				return nil, err
			}
			t.Providers[name] = p
		}
	}
	if l := f.Live; l != nil {
		for _, name := range order["live.providers"] {
			p, section := l.Providers[name], "live.providers."+name
			for key, value := range map[string]string{"endpoint": p.Endpoint, "model": p.Model, "api_key": p.APIKey} {
				if strings.TrimSpace(value) == "" {
					return nil, fmt.Errorf("%s.%s: required", section, key)
				}
			}
			u, err := url.Parse(p.Endpoint)
			if err != nil || u.Scheme != "wss" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || u.RawQuery != "" {
				return nil, invalid(text, section, "endpoint", "a wss:// URL without credentials, query or fragment")
			}
			if source, rest, _ := strings.Cut(p.APIKey, ":"); (source != "env" && source != "cmd") || strings.TrimSpace(rest) == "" {
				return nil, invalid(text, section, "api_key", "env:VARIABLE or cmd:COMMAND")
			}
			if p.InstructionsMode != "" && p.InstructionsMode != "inline" && p.InstructionsMode != "instructions" {
				return nil, invalid(text, section, "instructions_mode", "inline | instructions (system_instruction is not a realtime gateway field)")
			}
			if err := style(section, p.Style); err != nil {
				return nil, err
			}
		}
	}
	for _, service := range services {
		provider, ok := f.provider(service)
		if !ok {
			continue
		}
		active, err := activeProvider(text, service, provider, order[service+".providers"])
		if err != nil {
			return nil, err
		}
		c.Providers[service] = Providers{Names: order[service+".providers"]}
		c.resolve(service, active)
	}
	if !slices.Contains(PasteKeys, c.PasteKeys) {
		return nil, invalid(text, "general", "paste_keys", strings.Join(PasteKeys, " | "))
	}
	if !slices.Contains(LogLevels, c.LogLevel) {
		return nil, invalid(text, "general", "log_level", strings.Join(LogLevels, " | "))
	}
	if c.UploadIntervalMS < 50 || c.UploadIntervalMS > 1000 {
		return nil, invalid(text, "general", "upload_interval_ms", "an integer from 50 to 1000 milliseconds")
	}
	seconds := func(v float64) bool { return v > 0 && !math.IsInf(v, 0) } // also false for NaN
	if c.Split.Enabled && !seconds(c.Split.After) {
		return nil, invalid(text, "split", "after", "a finite number of seconds > 0")
	}
	if c.Split.Enabled && !seconds(c.Split.Pause) {
		return nil, invalid(text, "split", "pause", "a finite number of seconds > 0")
	}
	return c, nil
}

// checkConn validates one provider's connection keys and fills their defaults.
func checkConn(text string, md toml.MetaData, section string, conn *Conn) error {
	path := strings.Split(section, ".") // service, "providers", name
	for _, key := range []string{"endpoint", "api_key", "model"} {
		if !md.IsDefined(path[0], path[1], path[2], key) {
			return fmt.Errorf("%s.%s: required", section, key)
		}
	}
	conn.API = cmp.Or(conn.API, "openai")
	conn.AuthHeader = cmp.Or(conn.AuthHeader, "Authorization")
	conn.AuthScheme = cmp.Or(conn.AuthScheme, "bearer")
	if !slices.Contains(APIs, conn.API) {
		return invalid(text, section, "api", strings.Join(APIs, " | "))
	}
	if conn.API == "elevenlabs" && path[0] != "tts" {
		return fmt.Errorf("%s.api: elevenlabs supports tts only%s", section, at(lineOf(text, section, "api")))
	}
	if !slices.Contains(AuthSchemes, conn.AuthScheme) {
		return invalid(text, section, "auth_scheme", strings.Join(AuthSchemes, " | "))
	}
	if source, rest, _ := strings.Cut(conn.APIKey, ":"); (source != "env" && source != "cmd") || strings.TrimSpace(rest) == "" {
		return invalid(text, section, "api_key", "env:VARIABLE or cmd:COMMAND")
	}
	return nil
}

// provider is a service's `provider` key, and whether the service is in the file at all.
func (f *file) provider(service string) (string, bool) {
	switch {
	case service == "stt" && f.STT != nil:
		return f.STT.Provider, true
	case service == "llm" && f.LLM != nil:
		return f.LLM.Provider, true
	case service == "tts" && f.TTS != nil:
		return f.TTS.Provider, true
	case service == "live" && f.Live != nil:
		return f.Live.Provider, true
	}
	return "", false
}

// activeProvider is the named provider, else the only one.
func activeProvider(text, service, provider string, names []string) (string, error) {
	switch {
	case len(names) == 0:
		return "", fmt.Errorf("%s: no [%s.providers.<name>]", service, service)
	case provider != "" && !slices.Contains(names, provider):
		return "", invalid(text, service, "provider", "one of "+strings.Join(names, " | "))
	case provider != "":
		return provider, nil
	case len(names) == 1:
		return names[0], nil
	}
	return "", fmt.Errorf("%s.provider: required to choose between %s", service, strings.Join(names, " | "))
}

// resolve makes name, a validated provider, the active one of service.
func (c *Config) resolve(service, name string) {
	switch service {
	case "stt":
		p := c.file.STT.Providers[name]
		c.STT = &p
	case "llm":
		p := c.file.LLM.Providers[name]
		c.LLM = &p
	case "tts":
		p := c.file.TTS.Providers[name]
		p.Instructions = strings.TrimSpace(c.Styles[p.Style].Instructions)
		c.TTS = &p
	case "live":
		p := c.file.Live.Providers[name]
		p.Instructions = strings.TrimSpace(c.Styles[p.Style].Instructions)
		c.Live = &p
	}
	p := c.Providers[service]
	p.Active = name
	c.Providers[service] = p
}

// Use makes name the active provider of service for this run (--provider); config.toml is not written.
func (c *Config) Use(service, name string) error {
	p, ok := c.Providers[service]
	if !ok {
		return fmt.Errorf("--provider %s: no [%s] in config.toml", name, service)
	}
	if !slices.Contains(p.Names, name) {
		return fmt.Errorf("--provider %s: expected one of %s", name, strings.Join(p.Names, " | "))
	}
	c.resolve(service, name)
	return nil
}

// UseStyle makes name the style of service's active provider (tts or live) for this run (--style); "" applies no
// style, as in config.toml, which is not written.
func (c *Config) UseStyle(service, name string) error {
	if _, ok := c.Styles[name]; !ok && name != "" {
		names := strings.Join(slices.Sorted(maps.Keys(c.Styles)), " | ")
		return fmt.Errorf("--style %s: expected one of %s, or \"\" for no style", name, names)
	}
	instructions := strings.TrimSpace(c.Styles[name].Instructions)
	switch service {
	case "tts":
		c.TTS.Style, c.TTS.Instructions = name, instructions
	case "live":
		c.Live.Style, c.Live.Instructions = name, instructions
	}
	return nil
}

// UseTransform makes name the stt transform for this run (--transform); "" applies none. config.toml is not written.
func (c *Config) UseTransform(name string) error {
	if _, ok := c.Transform[name]; !ok && name != "" {
		names := strings.Join(slices.Sorted(maps.Keys(c.Transform)), " | ")
		return fmt.Errorf("--transform %s: expected one of %s, or \"\" for no transform", name, names)
	}
	c.STT.Transform = name
	return nil
}

func (c *Config) TransformModel(name string) string {
	if m := c.Transform[name].Model; m != "" {
		return m
	}
	if c.LLM == nil {
		return ""
	}
	return c.LLM.Model
}

// LiveDelivery is the active live provider's voice and style instructions; nothing without instructions_mode.
func (c *Config) LiveDelivery() Delivery {
	if c.Live == nil || c.Live.InstructionsMode == "" {
		return Delivery{}
	}
	return Delivery{Voice: voiceID(c.Live.Voices, c.Live.Voice), Instructions: c.Live.Instructions}
}

// VoiceEntry splits a voices entry, "ID" or "Name=ID" (at the first "="); an unnamed entry has an empty name.
func VoiceEntry(entry string) (name, id string) {
	if name, id, ok := strings.Cut(entry, "="); ok {
		return name, id
	}
	return "", entry
}

// voiceID is the ID of the voices entry that voice names; anything else is sent as written.
func voiceID(voices []string, voice string) string {
	for _, e := range voices {
		if name, id := VoiceEntry(e); name != "" && name == voice {
			return id
		}
	}
	return voice
}

func (t *TTS) Delivery() (voice, instructions, warning string) {
	if t.InstructionsMode == "" {
		if t.Style != "" {
			warning = "TTS styles are not applied: set instructions_mode in the tts provider"
			if t.API == "elevenlabs" {
				warning = "elevenlabs has no styles; pick an accent with the voice"
			}
		}
		return voiceID(t.Voices, t.Voice), "", warning
	}
	return voiceID(t.Voices, t.Voice), t.Instructions, ""
}

func invalid(text, section, key, expected string) error {
	name := key
	if section != "" {
		name = section + "." + key
	}
	return fmt.Errorf("%s: expected %s%s", name, expected, at(lineOf(text, section, key)))
}

func at(line int) string {
	if line == 0 {
		return ""
	}
	return fmt.Sprintf(" (at line %d)", line)
}

// lineOf is the line number of `key = …` inside [section] ("" = top level), or 0.
// ponytail: plain keys only; quoted or dotted keys get no line number.
func lineOf(text, section, key string) int {
	current := ""
	lines := strings.Split(text, "\n")
	inside := multiline(lines)
	for i, line := range lines {
		if inside[i] {
			continue
		}
		s := strings.TrimSpace(line)
		if strings.HasPrefix(s, "[") {
			if end := strings.Index(s, "]"); end > 0 {
				current = strings.TrimSpace(s[1:end])
			}
		} else if k, _, ok := strings.Cut(s, "="); ok && current == section && strings.TrimSpace(k) == key {
			return i + 1
		}
	}
	return 0
}

// multiline marks the lines inside a multi-line string, after the line that opens it. ponytail: counts """ / ”'
// per line; an escaped \""" inside a basic string miscounts.
func multiline(lines []string) []bool {
	inside := make([]bool, len(lines))
	delim := ""
	for i, line := range lines {
		if delim != "" {
			inside[i] = true
			if strings.Count(line, delim)%2 == 1 {
				delim = ""
			}
			continue
		}
		for _, d := range []string{`"""`, `'''`} {
			if strings.Count(line, d)%2 == 1 {
				delim = d
				break
			}
		}
	}
	return inside
}

// choices maps each tray-menu choice to its service (or general) and key; a provider key is "" and model, voice,
// language and transform belong to the service's active provider.
var choices = map[string][2]string{
	"stt_model":     {"stt", "model"},
	"llm_model":     {"llm", "model"},
	"tts_model":     {"tts", "model"},
	"tts_voice":     {"tts", "voice"},
	"live_voice":    {"live", "voice"},
	"language":      {"stt", "language"},
	"transform":     {"stt", "transform"},
	"stt_provider":  {"stt", ""},
	"llm_provider":  {"llm", ""},
	"tts_provider":  {"tts", ""},
	"live_provider": {"live", ""},
	"paste":         {"tray", "paste"},
	"paste_enter":   {"tray", "paste_enter"},
}

// SaveChoice writes one tray-menu choice (a string or bool) into config.toml, atomically, keeping every other line
// and the key's trailing comment. A file that would not load afterwards is left untouched and the error returned.
func SaveChoice(choice string, value any) error {
	// ponytail: read-modify-write without a lock; an editor saving at the same moment loses to whoever renames last.
	path, err := filepath.EvalSymlinks(Path()) // a symlinked config (dotfiles) keeps its link
	if err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	literal := fmt.Sprint(value)
	if s, ok := value.(string); ok {
		literal = strconv.Quote(s)
	}
	current, err := parse(string(data))
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	place, ok := choices[choice]
	if !ok {
		return fmt.Errorf("unknown choice %q", choice)
	}
	section, key := place[0], place[1]
	switch {
	case key == "":
		key = "provider"
	case section != "tray":
		p, ok := current.Providers[section]
		if !ok {
			return fmt.Errorf("%s: no [%s]", path, section)
		}
		section += ".providers." + p.Active
	}
	text := setKey(string(data), section, key, literal)
	if _, err := parse(text); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*.toml")
	if err != nil {
		return err
	}
	_, err = tmp.WriteString(text)
	if err2 := tmp.Chmod(info.Mode().Perm()); err == nil {
		err = err2
	}
	if err2 := tmp.Close(); err == nil {
		err = err2
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		os.Remove(tmp.Name())
	}
	return err
}

// setKey sets key = literal in [section] of the TOML text: in place when the key is there, else right under the
// header, else in a new section at the end.
func setKey(text, section, key, literal string) string {
	lines := strings.Split(text, "\n")
	inside := multiline(lines)
	in, header := false, -1
	for i, line := range lines {
		if inside[i] {
			continue
		}
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") {
			name, _, _ := strings.Cut(trimmed, "#")
			if in = strings.TrimSpace(name) == "["+section+"]"; in {
				header = i
			}
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !in || !ok || strings.TrimSpace(k) != key {
			continue
		}
		rest := strings.TrimLeft(v, " \t")
		lines[i] = k + "=" + v[:len(v)-len(rest)] + literal + rest[valueEnd(rest):]
		return strings.Join(lines, "\n")
	}
	if header < 0 {
		return strings.TrimRight(text, "\n") + "\n\n[" + section + "]\n" + key + " = " + literal + "\n"
	}
	return strings.Join(slices.Insert(lines, header+1, key+" = "+literal), "\n")
}

// valueEnd is the length of the one-line string or bare value that starts rest. SaveChoice never writes a
// multi-line value.
func valueEnd(rest string) int {
	switch {
	case rest == "":
		return 0
	case rest[0] == '"':
		for i := 1; i < len(rest); i++ {
			if rest[i] == '\\' {
				i++
			} else if rest[i] == '"' {
				return i + 1
			}
		}
		return len(rest)
	case rest[0] == '\'':
		if i := strings.IndexByte(rest[1:], '\''); i >= 0 {
			return i + 2
		}
		return len(rest)
	}
	if i := strings.IndexAny(rest, " \t#"); i >= 0 {
		return i
	}
	return len(rest)
}
