package hhapi

// T100 photo-surface wire tests: the album read parses the captured shape
// ({"images":[…]}, live 2026-10-09), the assignment posts the flat
// {"photo":[<id>]} body through the T51 edit route and verifies via the
// builder-state read-back.

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"hhauto/internal/config"
)

func newPhotoTestClient(t *testing.T, srvURL string) *Client {
	t.Helper()
	eps := epsCfg()
	eps.Site = srvURL
	c, err := NewClient(config.Account{DisplayName: "acc1", UserAgent: "ua/1"}, eps, nil, nil, WithBrowserTLSSpec(captureSpec(t)))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

func TestAlbumPhotoIDParsesProcessed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"images":[{"id":1,"state":"processing"},{"id":198680330,"state":"processed","artifactType":"RESUME_PHOTO"}]}`))
	}))
	defer srv.Close()
	id, err := newPhotoTestClient(t, srv.URL).AlbumPhotoID(context.Background())
	if err != nil {
		t.Fatalf("AlbumPhotoID: %v", err)
	}
	if id != 198680330 {
		t.Fatalf("id = %d, want the first processed image", id)
	}
}

func TestAlbumPhotoIDEmptyAlbumTyped(t *testing.T) {
	for name, body := range map[string]string{
		"no images":   `{"images":[]}`,
		"unprocessed": `{"images":[{"id":5,"state":"processing"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(body))
			}))
			defer srv.Close()
			if _, err := newPhotoTestClient(t, srv.URL).AlbumPhotoID(context.Background()); !errors.Is(err, ErrPhotoAlbumEmpty) {
				t.Fatalf("err = %v, want ErrPhotoAlbumEmpty", err)
			}
		})
	}
}

// TestSetResumePhotoPostsFlatContract pins the wire: the T51 edit route
// receives {"photo":[<id>]} (the flat integer array — the builder's
// [{"string":id}] wrapper is stripped), then the read-back gate accepts
// the assignment only when the builder field carries the id.
func TestSetResumePhotoPostsFlatContract(t *testing.T) {
	oldBudget, oldPoll := photoVerifyBudget, photoVerifyPoll
	photoVerifyBudget, photoVerifyPoll = 2*time.Second, time.Millisecond
	t.Cleanup(func() { photoVerifyBudget, photoVerifyPoll = oldBudget, oldPoll })

	var mu sync.Mutex
	var gotBody, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/applicant/resume/edit"):
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			gotBody, gotMethod = string(b), r.Method
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"resume":{"photo":[{"string":198680330}]},"_attributes":{"hash":"cl0ne1"}}`))
		case r.URL.Path == "/profile/shards/resume/get_by_hash":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"resume":{"photo":[{"string":198680330}]},"_attributes":{"hash":"cl0ne1"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := newPhotoTestClient(t, srv.URL)
	if err := c.SetResumePhoto(context.Background(), "cl0ne1", 198680330); err != nil {
		t.Fatalf("SetResumePhoto: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Fatalf("method = %s, want POST (T51 edit contract)", gotMethod)
	}
	if gotBody != `{"photo":[198680330]}` {
		t.Fatalf("wire body = %s, want the flat integer array", gotBody)
	}
}

func TestSetResumePhotoVerifyMismatchTyped(t *testing.T) {
	oldBudget, oldPoll := photoVerifyBudget, photoVerifyPoll
	photoVerifyBudget, photoVerifyPoll = 20*time.Millisecond, time.Millisecond
	t.Cleanup(func() { photoVerifyBudget, photoVerifyPoll = oldBudget, oldPoll })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(r.URL.Path, "/applicant/resume/edit") {
			_, _ = w.Write([]byte(`{"resume":{"photo":[]},"_attributes":{"hash":"cl0ne1"}}`))
			return
		}
		// The read-back never carries the id — the verify gate must trip.
		_, _ = w.Write([]byte(`{"resume":{"photo":[]},"_attributes":{"hash":"cl0ne1"}}`))
	}))
	defer srv.Close()

	c := newPhotoTestClient(t, srv.URL)
	if err := c.SetResumePhoto(context.Background(), "cl0ne1", 198680330); !errors.Is(err, ErrPhotoApply) {
		t.Fatalf("err = %v, want ErrPhotoApply", err)
	}
}
