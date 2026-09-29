package setka

import (
	"context"
	"errors"
	"fmt"

	"hhauto/internal/setka"
	"hhauto/internal/store"
)

// SendApproved executes the human's apply decision on a parked setka_dm
// review item (the approve mode of T61): sends the drafted text through the
// client and flips the ledger row to sent. A send failure returns as-is —
// the review item stays open for a retry; a processed row refuses loudly
// (the human decision must not double-send).
func SendApproved(ctx context.Context, client setka.SetkaClient, db *store.DB, dryRun bool, p *DMReviewPayload) (string, error) {
	if p == nil {
		return "", errors.New("setka: пустой payload ревью setka_dm")
	}
	row, err := db.SetkaDMGet(ctx, p.DMID)
	if err != nil {
		return "", err
	}
	if row == nil {
		return "", fmt.Errorf("setka: запись ЛС %d не найдена", p.DMID)
	}
	if row.Status != "queued_review" {
		return "", fmt.Errorf("setka: запись ЛС %d уже обработана (статус %s)", p.DMID, row.Status)
	}
	if dryRun {
		return "", nil
	}
	if err := client.SendDM(ctx, p.PersonID, p.Text); err != nil {
		return "", fmt.Errorf("setka: отправка ЛС %s не прошла: %w", p.PersonID, err)
	}
	if err := db.SetkaDMMarkSent(ctx, p.DMID); err != nil {
		return "", err
	}
	return "human: apply (setka dm sent)", nil
}

// DiscardApproved executes the human's discard decision: the parked DM is
// marked skipped and nothing is sent.
func DiscardApproved(ctx context.Context, db *store.DB, p *DMReviewPayload) error {
	if p == nil {
		return errors.New("setka: пустой payload ревью setka_dm")
	}
	row, err := db.SetkaDMGet(ctx, p.DMID)
	if err != nil {
		return err
	}
	if row == nil {
		return fmt.Errorf("setka: запись ЛС %d не найдена", p.DMID)
	}
	if row.Status != "queued_review" {
		return fmt.Errorf("setka: запись ЛС %d уже обработана (статус %s)", p.DMID, row.Status)
	}
	return db.SetkaDMMarkSkipped(ctx, p.DMID, "review discard")
}
