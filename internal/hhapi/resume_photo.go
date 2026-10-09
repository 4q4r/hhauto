package hhapi

// T100 resume photo surface (live contracts captured 2026-10-09): the
// account photo album is the artifact list behind the resume page's photo
// icon (GET /shards/applicant/artifacts/all → {"images":[…]}), and a
// resume renders its avatar from the per-resume builder field
// resume.photo — the album artifact id. An empty field renders the
// «Добавить фотографию» placeholder even though the account avatar
// (applicantProfileHeaderMenu.avatarUrl) is set: rendering is per-resume,
// not automatic.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ErrPhotoAlbumEmpty is the typed miss: the account album carries no
// processed image to assign (nothing to extract).
var ErrPhotoAlbumEmpty = errors.New("hhapi: account photo album has no processed image")

// ErrPhotoApply is the typed verify failure: the photo edit did not read
// back within the verify budget (hh applies saves asynchronously — T51).
var ErrPhotoApply = errors.New("hhapi: resume photo not applied")

// Verify-poll knobs are vars so the wire tests can shrink the budget (the
// production values carry T51 headroom: saves landed +2–4s live).
var (
	photoVerifyBudget = 60 * time.Second
	photoVerifyPoll   = 2 * time.Second
)

// AlbumPhotoID returns the id of the account's processed photo artifact —
// the avatar the resume page's photo icon loads. The first processed
// image wins; an album without one is the typed ErrPhotoAlbumEmpty.
func (c *Client) AlbumPhotoID(ctx context.Context) (int64, error) {
	ref := c.withPinnedHost(c.eps.site + "/applicant/me")
	resp, err := c.getXHR(ctx, "account_artifacts",
		c.endpointURL(epAccountArtifacts, c.eps.siteDefault("/shards/applicant/artifacts/all")), ref)
	if err != nil {
		return 0, err
	}
	var album struct {
		Images []struct {
			ID    int64  `json:"id"`
			State string `json:"state"`
		} `json:"images"`
	}
	if uerr := json.Unmarshal(resp.body, &album); uerr != nil {
		return 0, fmt.Errorf("album artifacts: decode: %w", uerr)
	}
	for _, img := range album.Images {
		if img.State == "processed" {
			return img.ID, nil
		}
	}
	return 0, ErrPhotoAlbumEmpty
}

// SetResumePhoto assigns an existing album photo to a resume through the
// T51 edit contract: the flat {"photo":[<id>]} body (editFlatValue strips
// the builder's conditional wrapper). The read-back poll is the verify
// gate — success is claimed only when the builder field carries the id.
func (c *Client) SetResumePhoto(ctx context.Context, resumeHash string, photoID int64) error {
	body := fmt.Sprintf(`[{"string":%d}]`, photoID)
	if err := c.postResumeEdit(ctx, resumeHash, "photo", json.RawMessage(body)); err != nil {
		return err
	}
	deadline := time.Now().Add(photoVerifyBudget)
	for {
		st, err := c.fetchBuilderState(ctx, resumeHash)
		if err == nil {
			var got []struct {
				String int64 `json:"string"`
			}
			if jerr := json.Unmarshal(st.Resume["photo"], &got); jerr == nil && len(got) == 1 && got[0].String == photoID {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%w: %s", ErrPhotoApply, resumeHash)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w: %s: %v", ErrPhotoApply, resumeHash, ctx.Err())
		case <-time.After(photoVerifyPoll):
		}
	}
}
