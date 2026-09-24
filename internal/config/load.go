package config

import (
	"fmt"
	"os"
	"strings"

	toml "github.com/pelletier/go-toml/v2"
)

// Load reads the TOML file at path and returns a fully defaulted, validated
// configuration. Preprocessing happens on the raw text, in order:
//
//  1. {data_dir} placeholders are replaced with [general].data_dir;
//  2. a leading "~" in string values is expanded to the home directory;
//  3. ${ENV} placeholders are replaced (an unset variable is an error
//     that names the variable). Expansion applies to the entire document,
//     including TOML comments — a ${VAR} inside a comment is expanded too.
//
// After decoding, applyDefaults fills unset values and Validate enforces
// referential integrity. Every stage fails fast with a contextual message.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	text := string(raw)

	if dataDir := probeDataDir(text); dataDir != "" {
		text = strings.ReplaceAll(text, "{data_dir}", dataDir)
	}
	text = expandTildeText(text)
	text, err = expandEnv(text)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	var cfg Config
	if err := toml.Unmarshal([]byte(text), &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	applyDefaults(&cfg)
	if err := Validate(&cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &cfg, nil
}

// dataDirProbe is a minimal decode target used to find [general].data_dir
// before the full decode, so that {data_dir} placeholders elsewhere in the
// document can be substituted textually.
type dataDirProbe struct {
	General struct {
		DataDir string `toml:"data_dir"`
	} `toml:"general"`
}

func probeDataDir(text string) string {
	var p dataDirProbe
	if err := toml.Unmarshal([]byte(text), &p); err != nil {
		return "" // a malformed document is reported by the full decode
	}
	return p.General.DataDir
}
