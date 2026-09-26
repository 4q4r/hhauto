package config

// T30: [features.autoclone.improve.editor] — the browser (chromedp) apply
// of the improve plan: URL-pattern overrides, the cookie domain and the
// page/save timeouts.
import (
	"strings"
	"testing"
)

func baseImproveEditor() AutoCloneImprove {
	return AutoCloneImprove{
		Enabled:      true,
		Fields:       []string{"title"},
		ModelProfile: "main",
		Editor: AutoCloneImproveEditor{
			Enabled: true,
		},
	}
}

func TestEditorDefaults(t *testing.T) {
	var c Config
	applyDefaults(&c)
	ed := c.Features.AutoClone.Improve.Editor
	if ed.BaseURL != "https://hh.ru" {
		t.Fatalf("default editor base_url = %q, want https://hh.ru", ed.BaseURL)
	}
	if ed.CookieDomain != ".hh.ru" {
		t.Fatalf("default editor cookie_domain = %q, want .hh.ru", ed.CookieDomain)
	}
	if ed.PageTimeoutSec != 120 {
		t.Fatalf("default page_timeout_sec = %d, want 120", ed.PageTimeoutSec)
	}
	if ed.SaveTimeoutSec != 20 {
		t.Fatalf("default save_timeout_sec = %d, want 20", ed.SaveTimeoutSec)
	}
	if ed.Enabled {
		t.Fatal("editor must default to disabled (nil-runner degradation stays the default)")
	}
}

func TestValidateEditorTemplates(t *testing.T) {
	c := baseAutoCloneConfig()
	c.Features.AutoClone.Improve = baseImproveEditor()
	if err := Validate(c); err != nil {
		t.Fatalf("valid editor config rejected: %v", err)
	}

	// A screen override without the {resume} placeholder cannot address the
	// clone — fail fast at config time.
	c.Features.AutoClone.Improve.Editor.ScreenURLs = map[string]string{
		"title": "/wizard?screen={screen}",
	}
	err := Validate(c)
	if err == nil {
		t.Fatal("screen override without {resume} must be rejected")
	}
	if !strings.Contains(err.Error(), "{resume}") {
		t.Fatalf("error must name the missing placeholder: %v", err)
	}

	// A template addressing experience items must carry {item}.
	c.Features.AutoClone.Improve.Editor.ScreenURLs = map[string]string{
		"experience": "/wizard/{resume}/{screen}",
	}
	if err := Validate(c); err == nil {
		t.Fatal("experience override without {item} must be rejected")
	}
	c.Features.AutoClone.Improve.Editor.ScreenURLs = map[string]string{
		"experience": "/wizard/{resume}/{screen}/item/{item}",
	}
	if err := Validate(c); err != nil {
		t.Fatalf("complete experience override rejected: %v", err)
	}
}

func TestValidateEditorTimeouts(t *testing.T) {
	c := baseAutoCloneConfig()
	c.Features.AutoClone.Improve = baseImproveEditor()
	c.Features.AutoClone.Improve.Editor.PageTimeoutSec = 0 // unset → default
	if err := Validate(c); err != nil {
		t.Fatalf("unset timeouts ride the defaults: %v", err)
	}
	c.Features.AutoClone.Improve.Editor.PageTimeoutSec = 2
	if err := Validate(c); err == nil {
		t.Fatal("page_timeout_sec=2 is below the sane floor and must be rejected")
	}
	c.Features.AutoClone.Improve.Editor.PageTimeoutSec = 60
	c.Features.AutoClone.Improve.Editor.SaveTimeoutSec = 1
	if err := Validate(c); err == nil {
		t.Fatal("save_timeout_sec=1 is below the sane floor and must be rejected")
	}
}

// A disabled editor section may stay half-filled (mirrors the improve
// section's disabled tolerance).
func TestValidateEditorDisabledTolerant(t *testing.T) {
	c := baseAutoCloneConfig()
	c.Features.AutoClone.Improve = AutoCloneImprove{
		Enabled: true, Fields: []string{"title"}, ModelProfile: "main",
		Editor: AutoCloneImproveEditor{Enabled: false, ScreenURLs: map[string]string{"x": "y"}},
	}
	if err := Validate(c); err != nil {
		t.Fatalf("disabled editor must not be validated: %v", err)
	}
}
