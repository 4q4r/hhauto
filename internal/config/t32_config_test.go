package config

// T32: [browser] — the shared browser subsystem: executable discovery with
// cloakbrowser auto-download (auto_download), the humanize launch profile
// (humanize + preset) and the binary release channel (channel).
import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBrowserDefaults(t *testing.T) {
	var c Config
	applyDefaults(&c)
	b := c.Browser
	if b.AutoDownload == nil || !*b.AutoDownload {
		t.Fatal("default browser.auto_download must be true (nil → true, R1)")
	}
	if b.Humanize == nil || !*b.Humanize {
		t.Fatal("default browser.humanize must be true (nil → true, R1)")
	}
	if b.Preset != "careful" {
		t.Fatalf("default browser.preset = %q, want careful", b.Preset)
	}
	if b.Channel != "auto" {
		t.Fatalf("default browser.channel = %q, want auto", b.Channel)
	}
}

func TestBrowserExplicitFalseRespected(t *testing.T) {
	var c Config
	c.Browser.AutoDownload = new(bool)
	c.Browser.Humanize = new(bool)
	applyDefaults(&c)
	if *c.Browser.AutoDownload {
		t.Fatal("explicit auto_download=false must be respected")
	}
	if *c.Browser.Humanize {
		t.Fatal("explicit humanize=false must be respected")
	}
}

func TestValidateBrowser(t *testing.T) {
	c := validConfig()
	if err := Validate(c); err != nil {
		t.Fatalf("zero-value [browser] must validate: %v", err)
	}

	for _, tc := range []struct {
		name string
		mut  func(*Config)
		want string
	}{
		{
			name: "unknown preset",
			mut:  func(c *Config) { c.Browser.Preset = "aggressive" },
			want: "browser.preset",
		},
		{
			name: "unknown channel",
			mut:  func(c *Config) { c.Browser.Channel = "beta" },
			want: "browser.channel",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := validConfig()
			tc.mut(c)
			err := Validate(c)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate err = %v, want contains %q", err, tc.want)
			}
		})
	}

	c = validConfig()
	c.Browser.Preset = "default"
	c.Browser.Channel = "pro"
	if err := Validate(c); err != nil {
		t.Fatalf("explicit default/pro must validate: %v", err)
	}
}

func TestBrowserLoadTOML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hhauto.toml")
	// Minimal document: only the [browser] section is asserted; the rest
	// must default cleanly (a minimal TOML yields a working config).
	doc := `
[browser]
auto_download = false
preset = "default"
channel = "free"
`
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if *c.Browser.AutoDownload {
		t.Fatal("auto_download=false must survive the load")
	}
	if !*c.Browser.Humanize {
		t.Fatal("humanize must default to true when the key is absent")
	}
	if c.Browser.Preset != "default" || c.Browser.Channel != "free" {
		t.Fatalf("preset/channel = %q/%q, want default/free", c.Browser.Preset, c.Browser.Channel)
	}
}
