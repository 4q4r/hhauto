//go:build live100

package hhapi

// T100 LIVE PROBE (manual, network + owner session required; never in the
// default gate):
//
//	go test -tags live100 -run TestLive100PhotoProbe -count=1 -v ./internal/hhapi/
//	go test -tags live100 -run TestLive100PhotoApply -count=1 -v ./internal/hhapi/
//
// Probe is read-only: the account photo album (the XHR behind the photo
// icon), the resume shard's photoUrls, the builder-state photo field of
// every resume and the /applicant/me SSR photo panel. Apply assigns the
// existing album photo id to resumes whose builder photo field is empty
// via the T51 edit contract (no upload — the photo already lives on hh).
// Dumps land in /tmp/opencode/t100/.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"hhauto/internal/config"
	"hhauto/internal/tlsfp"
)

const live100DumpDir = "/tmp/opencode/t100"

func live100Client(t *testing.T) *Client {
	t.Helper()
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
	raw, err := os.ReadFile("/home/whoami/.local/share/hhauto/cookies/main.bin")
	if err != nil {
		t.Fatalf("cookie store: %v", err)
	}
	var blob struct {
		Cookies []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"cookies"`
	}
	if err := json.Unmarshal(raw, &blob); err != nil {
		t.Fatalf("cookie store decode: %v", err)
	}
	cks := make([]*http.Cookie, 0, len(blob.Cookies))
	for _, ck := range blob.Cookies {
		cks = append(cks, &http.Cookie{Name: ck.Name, Value: ck.Value})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if err := c.LoadCookies(ctx, cks); err != nil {
		t.Fatalf("load cookies: %v", err)
	}
	return c
}

func live100Dump(t *testing.T, name string, body []byte) {
	t.Helper()
	if err := os.MkdirAll(live100DumpDir, 0o755); err != nil {
		t.Fatalf("dump dir: %v", err)
	}
	p := filepath.Join(live100DumpDir, name)
	if err := os.WriteFile(p, body, 0o644); err != nil {
		t.Fatalf("dump %s: %v", name, err)
	}
}

// live100AlbumPhotoID extracts the first numeric image id from the
// artifacts payload ({images:[…]} per the reverse-engineered map; the scan
// tolerates other nestings).
func live100AlbumPhotoID(raw []byte) int64 {
	var aux struct {
		Images []struct {
			ID int64 `json:"id"`
		} `json:"images"`
	}
	if err := json.Unmarshal(raw, &aux); err == nil && len(aux.Images) > 0 {
		return aux.Images[0].ID
	}
	return 0
}

// live100ShardPhotos maps resumeHash -> photoUrls length from the raw
// mine-shard JSON (entries carry _attributes.hash; photoUrls may sit in
// _attributes or fields — the scan checks both).
func live100ShardPhotos(t *testing.T, raw []byte) map[string]int {
	t.Helper()
	var payload struct {
		Resumes []json.RawMessage `json:"resumes"`
		Items   []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("shard decode: %v", err)
	}
	raws := payload.Resumes
	if raws == nil {
		raws = payload.Items
	}
	out := map[string]int{}
	for _, r := range raws {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(r, &m); err != nil {
			continue
		}
		var attrs map[string]json.RawMessage
		_ = json.Unmarshal(m["_attributes"], &attrs)
		hash := ""
		_ = json.Unmarshal(attrs["hash"], &hash)
		if hash == "" {
			_ = json.Unmarshal(m["resumeHash"], &hash)
		}
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(m["fields"], &fields)
		n := 0
		for _, src := range []json.RawMessage{attrs["photoUrls"], fields["photoUrls"]} {
			var urls []json.RawMessage
			if json.Unmarshal(src, &urls) == nil && len(urls) > n {
				n = len(urls)
			}
		}
		out[hash] = n
	}
	return out
}

func live100FetchAlbum(t *testing.T, c *Client, ctx context.Context) []byte {
	t.Helper()
	ref := c.withPinnedHost(c.eps.site + "/applicant/me")
	album, err := c.getXHR(ctx, "t100_artifacts", c.eps.siteDefault("/shards/applicant/artifacts/all"), ref)
	if err != nil {
		t.Fatalf("artifacts/all: %v", err)
	}
	live100Dump(t, "artifacts.json", album.body)
	t.Logf("artifacts/all: status=%d len=%d body=%s", album.status, len(album.body), snippet(album.body))
	return album.body
}

func TestLive100PhotoProbe(t *testing.T) {
	c := live100Client(t)
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()
	// 1. The photo album.
	album := live100FetchAlbum(t, c, ctx)
	t.Logf("album photo id: %d", live100AlbumPhotoID(album))

	// 2. The mine-shard: photoUrls per resume.
	ref := c.withPinnedHost(c.eps.site + "/applicant/resumes")
	shard, err := c.getXHR(ctx, "t100_shard", c.endpointURL(epResumes, c.eps.siteDefault("/shards/applicant/resumes")), ref)
	if err != nil {
		t.Fatalf("shard: %v", err)
	}
	live100Dump(t, "shard.json", shard.body)
	photos := live100ShardPhotos(t, shard.body)
	hashes := make([]string, 0, len(photos))
	for h := range photos {
		hashes = append(hashes, h)
	}
	sort.Strings(hashes)
	for _, h := range hashes {
		t.Logf("shard %-8s photoUrls=%d", h[:min(8, len(h))], photos[h])
	}

	// 3. Builder-state photo field per resume (full dump for the
	// offline invariance diff against the morning t104 captures).
	for _, h := range hashes {
		st, err := c.fetchBuilderState(ctx, h)
		if err != nil {
			t.Errorf("get_by_hash %s: %v", h[:8], err)
			continue
		}
		live100Dump(t, "final-state-"+h[:8]+".json", []byte(`{"hash":"`+st.Hash+`",`+
			`"title":`+string(st.Resume["title"])+`,`+
			`"photo":`+string(st.Resume["photo"])+`,`+
			`"language":`+string(st.Resume["language"])+`,`+
			`"experience":`+string(st.ProfileFields["experience"])+`}`))
		t.Logf("state %-8s photo=%s title=%s", h[:8], string(st.Resume["photo"]), st.Title())
	}

	// 4. The /applicant/me SSR document: the "Добавить фотографию" panel.
	doc, err := c.getDoc(ctx, "t100_me", c.withPinnedHost(c.eps.site+"/applicant/me"), c.withPinnedHost(c.eps.site+"/"))
	if err != nil {
		t.Logf("applicant/me failed: %v", err)
		return
	}
	live100Dump(t, "me.html", doc.body)
	t.Logf("applicant/me: status=%d len=%d", doc.status, len(doc.body))
	if state, ok := initialState(doc.body); ok {
		keys := make([]string, 0, len(state))
		for k := range state {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			lk := strings.ToLower(k)
			if strings.Contains(lk, "photo") || strings.Contains(lk, "artifact") || strings.Contains(lk, "avatar") {
				raw, _ := json.Marshal(state[k])
				t.Logf("me state %s = %s", k, string(raw))
			}
		}
	} else {
		t.Logf("applicant/me: no HH-Lux initialState (captcha/redirect page — check me.html)")
	}
}

// TestLive100RenderCheck dumps the own-resume SSR page for one resume with
// a photo and one without, proving which markup the avatar renders from.
func TestLive100RenderCheck(t *testing.T) {
	c := live100Client(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	for _, tc := range []struct {
		name, hash string
	}{
		{"fixed-qa-068e9047", "068e9047ff113b0ec00039ed1f357a626b314e"},
		{"ref-with-photo-fe31e03e", "fe31e03eff110666c30039ed1f4f5a59327851"},
	} {
		doc, err := c.getDoc(ctx, "t100_resume_render", c.withPinnedHost(c.eps.site+"/resume/"+tc.hash), c.withPinnedHost(c.eps.site+"/"))
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		live100Dump(t, "render-"+tc.name+".html", doc.body)
		page := string(doc.body)
		own := live100OwnPhotoNode(page)
		t.Logf("%s: status=%d len=%d own-photo-node=%q", tc.name, doc.status, len(doc.body), own)
	}
}

// live100OwnPhotoNode extracts the resume page's own embedded photo node
// (the escaped HH-Lux state) — the render truth for the avatar widget.
func live100OwnPhotoNode(page string) string {
	for _, pat := range []string{"&#34;photo&#34;:[", "&quot;photo&quot;:[", `"photo":[`} {
		i := strings.Index(page, pat)
		if i < 0 {
			continue
		}
		j := strings.Index(page[i:], "]")
		if j < 0 {
			continue
		}
		return page[i : i+j+1]
	}
	return "(not found)"
}

func TestLive100PhotoApply(t *testing.T) {
	c := live100Client(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()

	album := live100FetchAlbum(t, c, ctx)
	photoID := live100AlbumPhotoID(album)
	if photoID == 0 {
		t.Fatalf("no photo id in album payload")
	}
	t.Logf("album photo id: %d", photoID)

	ref := c.withPinnedHost(c.eps.site + "/applicant/resumes")
	shard, err := c.getXHR(ctx, "t100_shard", c.endpointURL(epResumes, c.eps.siteDefault("/shards/applicant/resumes")), ref)
	if err != nil {
		t.Fatalf("shard: %v", err)
	}
	photos := live100ShardPhotos(t, shard.body)
	hashes := make([]string, 0, len(photos))
	for h := range photos {
		hashes = append(hashes, h)
	}
	sort.Strings(hashes)

	for _, h := range hashes {
		st, err := c.fetchBuilderState(ctx, h)
		if err != nil {
			t.Errorf("get_by_hash %s: %v", h[:8], err)
			continue
		}
		var cur []struct {
			String int64 `json:"string"`
		}
		_ = json.Unmarshal(st.Resume["photo"], &cur)
		if len(cur) > 0 && cur[0].String != 0 {
			t.Logf("%-8s already photo=%v — skip", h[:8], cur)
			continue
		}
		body := fmt.Sprintf(`[{"string":%d}]`, photoID)
		if err := c.postResumeEdit(ctx, h, "photo", json.RawMessage(body)); err != nil {
			t.Errorf("%-8s photo POST: %v", h[:8], err)
			continue
		}
		t.Logf("%-8s photo POST sent -> re-read", h[:8])
		st2, err := c.fetchBuilderState(ctx, h)
		if err != nil {
			t.Errorf("re-read %s: %v", h[:8], err)
			continue
		}
		t.Logf("%-8s after: photo=%s", h[:8], string(st2.Resume["photo"]))
	}
}
