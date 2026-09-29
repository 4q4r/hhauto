//go:build live58

package hhapi

// T58 stability debug: read the clone's experience pool N times and log
// desc_sha + parent resumes order per read — distinguishes hh-side async
// reverts from replica lag.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"

	"hhauto/internal/config"
	"hhauto/internal/tlsfp"
)

func TestLive58StabilityDebug(t *testing.T) {
	hash := "4f48f357ff112c2fcc0039ed1f6c7555414349"
	cfg, err := config.Load("/home/whoami/.config/hhauto/hhauto.toml")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	var acct config.Account
	for _, a := range cfg.Accounts {
		acct = a
	}
	spec, err := tlsfp.Load(tlsfp.DefaultPath(cfg.General.DataDir))
	if err != nil {
		t.Fatalf("tls spec: %v", err)
	}
	if spec.UserAgent != "" {
		acct.UserAgent = spec.UserAgent
	}
	c, err := NewClient(acct, cfg.API.Endpoints, nil, nil, WithBrowserTLSSpec(spec))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	rawCk, err := os.ReadFile("/home/whoami/.local/share/hhauto/cookies/main.bin")
	if err != nil {
		t.Fatalf("cookie store: %v", err)
	}
	var blob struct {
		Cookies []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"cookies"`
	}
	if err := json.Unmarshal(rawCk, &blob); err != nil {
		t.Fatalf("cookie store decode: %v", err)
	}
	cks := make([]*http.Cookie, 0, len(blob.Cookies))
	for _, ck := range blob.Cookies {
		cks = append(cks, &http.Cookie{Name: ck.Name, Value: ck.Value})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Second)
	defer cancel()
	if err := c.LoadCookies(ctx, cks); err != nil {
		t.Fatalf("load cookies: %v", err)
	}

	read := func(tag string) {
		st, err := c.fetchBuilderState(ctx, hash)
		if err != nil {
			t.Fatalf("[%s] builder state: %v", tag, err)
		}
		pool, err := experiencePool(st)
		if err != nil {
			t.Fatalf("[%s] pool: %v", tag, err)
		}
		for _, rec := range pool {
			var r struct {
				ID          int64  `json:"id"`
				Description string `json:"description"`
				Resumes     []struct {
					Hash string `json:"hash"`
				} `json:"resumes"`
			}
			if err := json.Unmarshal(rec.Raw, &r); err != nil {
				t.Fatalf("[%s] decode: %v", tag, err)
			}
			kind := "own"
			if len(r.Resumes) != 1 || r.Resumes[0].Hash != hash {
				kind = "par"
			}
			sum := sha256.Sum256([]byte(r.Description))
			t.Logf("[%s] id=%d %s len=%d sha=%s nres=%d", tag, r.ID, kind, len(r.Description), hex.EncodeToString(sum[:])[:12], len(r.Resumes))
		}
	}
	for i := 1; i <= 6; i++ {
		read("r" + string(rune('0'+i)))
		if i < 6 {
			time.Sleep(25 * time.Second)
		}
	}
}
