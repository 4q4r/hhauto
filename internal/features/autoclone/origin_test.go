package autoclone

// T29 clone_original_action tests: keep (default) does nothing, hide flips
// visibility via the hh hide endpoint, delete removes the resume; any
// unknown action or failed mutation degrades to keep with a WARN
// (fail-safe — the source resume is never lost to an automation bug).
import (
	"context"
	"errors"
	"testing"

	"hhauto/internal/hhapi"
)

type fakeOriginAPI struct {
	hides, shows, deletes []string
	hideErr, deleteErr    error
}

func (f *fakeOriginAPI) HideResume(_ context.Context, hash string) error {
	if f.hideErr != nil {
		return f.hideErr
	}
	f.hides = append(f.hides, hash)
	return nil
}

func (f *fakeOriginAPI) ShowResume(_ context.Context, hash string) error {
	f.shows = append(f.shows, hash)
	return nil
}

func (f *fakeOriginAPI) DeleteResume(_ context.Context, hash string) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.deletes = append(f.deletes, hash)
	return nil
}

var _ OriginMutator = (*fakeOriginAPI)(nil)

func TestOriginActionKeepDoesNothing(t *testing.T) {
	api := &fakeOriginAPI{}
	ApplyOriginAction(context.Background(), api, "keep", "parent1", testLoggerT())
	ApplyOriginAction(context.Background(), api, "", "parent1", testLoggerT()) // unset → keep
	if len(api.hides)+len(api.deletes) != 0 {
		t.Fatal("keep must not touch the resume")
	}
}

func TestOriginActionHide(t *testing.T) {
	api := &fakeOriginAPI{}
	ApplyOriginAction(context.Background(), api, "hide", "parent1", testLoggerT())
	if len(api.hides) != 1 || api.hides[0] != "parent1" {
		t.Fatalf("hides = %v", api.hides)
	}
	if len(api.deletes) != 0 {
		t.Fatal("hide must never delete")
	}
}

func TestOriginActionDelete(t *testing.T) {
	api := &fakeOriginAPI{}
	ApplyOriginAction(context.Background(), api, "delete", "parent1", testLoggerT())
	if len(api.deletes) != 1 || api.deletes[0] != "parent1" {
		t.Fatalf("deletes = %v", api.deletes)
	}
	if len(api.hides) != 0 {
		t.Fatal("delete must not hide first")
	}
}

// Fail-safe: a failed hide/delete degrades to keep (logged), never a hard
// error that would break the clone run.
func TestOriginActionFailureDegradesToKeep(_ *testing.T) {
	api := &fakeOriginAPI{hideErr: errors.New("boom")}
	ApplyOriginAction(context.Background(), api, "hide", "parent1", testLoggerT())
	api2 := &fakeOriginAPI{deleteErr: errors.New("boom")}
	ApplyOriginAction(context.Background(), api2, "delete", "parent1", testLoggerT())
}

// Unknown action: WARN + keep.
func TestOriginActionUnknown(t *testing.T) {
	api := &fakeOriginAPI{}
	ApplyOriginAction(context.Background(), api, "explode", "parent1", testLoggerT())
	if len(api.hides)+len(api.deletes) != 0 {
		t.Fatal("unknown action must do nothing")
	}
}

// The vocabulary the multiapply hook switches on.
func TestOriginActionNormalization(t *testing.T) {
	for _, action := range []string{"keep", "hide", "delete"} {
		if !validOriginAction(action) {
			t.Fatalf("%q must be valid", action)
		}
	}
	if validOriginAction("explode") || validOriginAction("") {
		t.Fatal("empty/unknown actions are invalid (empty is handled as keep by the hook)")
	}
	var _ = hhapi.ErrCloneMaterialize
}
