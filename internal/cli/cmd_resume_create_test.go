package cli

// T47 `hhauto resume create` unit tests: account resolution (deterministic,
// never map-order dependent) and the shard-verified verdict rendering
// (including the honest untitled-shell note).

import (
	"bytes"
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"hhauto/internal/config"
	"hhauto/internal/hhapi"
)

// newTestOut builds the buffered output pair the verdict tests assert on.
func newTestOut() (out, *bytes.Buffer) {
	var buf bytes.Buffer
	return out{stdout: &buf, stderr: &buf}, &buf
}

func TestResolveCreateAccountExplicitFlag(t *testing.T) {
	rt := &Runtime{Cfg: &config.Config{Accounts: map[string]config.Account{
		"main": {}, "other": {},
	}}}
	id, err := resolveCreateAccount(rt, "other")
	if err != nil || id != "other" {
		t.Fatalf("id=%q err=%v", id, err)
	}
}

func TestResolveCreateAccountSingleIsDefault(t *testing.T) {
	rt := &Runtime{Cfg: &config.Config{Accounts: map[string]config.Account{"main": {}}}}
	id, err := resolveCreateAccount(rt, "")
	if err != nil || id != "main" {
		t.Fatalf("id=%q err=%v", id, err)
	}
}

func TestResolveCreateAccountAmbiguousRequiresFlag(t *testing.T) {
	rt := &Runtime{Cfg: &config.Config{Accounts: map[string]config.Account{
		"main": {}, "other": {},
	}}}
	if _, err := resolveCreateAccount(rt, ""); err == nil {
		t.Fatal("two accounts without --account must fail")
	}
	if _, err := resolveCreateAccount(rt, "ghost"); err == nil {
		t.Fatal("unknown account must fail")
	}
}

// fakeCreateAPI satisfies HHAPI through embedding; only ListResumes runs.
type fakeCreateAPI struct {
	hhapi.HHAPI
	resumes []hhapi.Resume
	err     error
}

func (f fakeCreateAPI) ListResumes(_ context.Context) ([]hhapi.Resume, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.resumes, nil
}

func TestPrintCreateVerdictRendersVerifiedRow(t *testing.T) {
	api := fakeCreateAPI{resumes: []hhapi.Resume{
		{HHID: "newh4sh", Title: "", Status: "not_finished"},
	}}
	o, buf := newTestOut()
	if err := printCreateVerdict(context.Background(), o, api, "main", "", "newh4sh"); err != nil {
		t.Fatalf("printCreateVerdict: %v", err)
	}
	if !strings.Contains(buf.String(), "newh4sh") || !strings.Contains(buf.String(), "not_finished") {
		t.Fatalf("verdict %q misses the verified row", buf.String())
	}
}

// T100 photo step: the owner default (nil = on) applies the album photo
// to the fresh shell; a step failure degrades to a stderr warning (the
// create stands); the config off-switch skips silently.
type fakePhotoAPI struct {
	hhapi.HHAPI
	albumID   int64
	albumErr  error
	setErr    error
	setHashes []string
}

func (f *fakePhotoAPI) AlbumPhotoID(context.Context) (int64, error) { return f.albumID, f.albumErr }

func (f *fakePhotoAPI) SetResumePhoto(_ context.Context, hash string, id int64) error {
	if f.setErr != nil {
		return f.setErr
	}
	f.setHashes = append(f.setHashes, hash+":"+strconv.FormatInt(id, 10))
	return nil
}

func TestApplyCreatePhotoAppliesOnDefault(t *testing.T) {
	api := &fakePhotoAPI{albumID: 198680330}
	rt := &Runtime{Cfg: &config.Config{Features: config.Features{}}}
	o, buf := newTestOut()
	applyCreatePhoto(context.Background(), o, rt, api, "newh4sh")
	if len(api.setHashes) != 1 || api.setHashes[0] != "newh4sh:198680330" {
		t.Fatalf("photo not applied: %v", api.setHashes)
	}
	if !strings.Contains(buf.String(), "Фото профиля применено") {
		t.Fatalf("output %q misses the success line", buf.String())
	}
}

func TestApplyCreatePhotoFailureWarnsNotFails(t *testing.T) {
	api := &fakePhotoAPI{albumErr: hhapi.ErrPhotoAlbumEmpty}
	rt := &Runtime{Cfg: &config.Config{Features: config.Features{}}}
	o, buf := newTestOut()
	applyCreatePhoto(context.Background(), o, rt, api, "newh4sh") // must not panic/exit
	if strings.Contains(buf.String(), "Фото профиля применено") {
		t.Fatalf("success line on a failed step: %q", buf.String())
	}
	if !strings.Contains(buf.String(), "фото профиля") {
		t.Fatalf("the failure stayed silent: %q", buf.String())
	}
}

func TestApplyCreatePhotoOffSwitchSkips(t *testing.T) {
	off := false
	api := &fakePhotoAPI{albumID: 1}
	rt := &Runtime{Cfg: &config.Config{Features: config.Features{ApplyProfilePhoto: &off}}}
	o, buf := newTestOut()
	applyCreatePhoto(context.Background(), o, rt, api, "newh4sh")
	if len(api.setHashes) != 0 || buf.String() != "" {
		t.Fatalf("off-switch leaked: hashes=%v out=%q", api.setHashes, buf.String())
	}
}

func TestPrintCreateVerdictNotesUntitleShell(t *testing.T) {
	api := fakeCreateAPI{resumes: []hhapi.Resume{
		{HHID: "newh4sh", Title: "", Status: "not_finished"},
	}}
	o, buf := newTestOut()
	if err := printCreateVerdict(context.Background(), o, api, "main", "Инженер", "newh4sh"); err != nil {
		t.Fatalf("printCreateVerdict: %v", err)
	}
	if !strings.Contains(buf.String(), "визард") {
		t.Fatalf("verdict %q must note the wizard-owned content", buf.String())
	}
}

func TestPrintCreateVerdictVerifyFailure(t *testing.T) {
	api := fakeCreateAPI{err: errors.New("boom")}
	o, _ := newTestOut()
	if err := printCreateVerdict(context.Background(), o, api, "main", "", "newh4sh"); err == nil {
		t.Fatal("verify read failure must fail the command")
	}
}
