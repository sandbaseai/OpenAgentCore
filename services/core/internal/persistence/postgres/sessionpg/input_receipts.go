package sessionpg

import (
	"context"
	"errors"
	"fmt"

	v1 "github.com/MiniMax-AI/OpenAgentCore/contracts/agents-api/v1"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/db/sqlc"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/items"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/persistence/postgres/pgunit"
	"github.com/MiniMax-AI/OpenAgentCore/services/core/internal/sessions"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// GetInputReceipts uses the same immutable source projection as admission.
// Input rows and their Items commit atomically; no executor observation is needed.
func (s *Store) GetInputReceipts(ctx context.Context, tenantID, sessionID, key string) (v1.SessionInputReceipts, error) {
	result := v1.SessionInputReceipts{Object: "agent.session.input_receipts", SessionID: sessionID, Receipts: []v1.SessionInputReceipt{}}
	tenant, err := parseID(tenantID)
	if err != nil {
		return result, err
	}
	session := pgunit.PathID(sessionID)
	err = s.units.Snapshot(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := sqlc.New(tx)
		if err := visibleSession(ctx, q, tenant, session); err != nil {
			return err
		}
		if err := sessions.ValidateInputKey(key); err != nil {
			return err
		}
		rows, err := q.ReadInputReceiptBatch(ctx, sqlc.ReadInputReceiptBatchParams{SessionID: session, IdempotencyKey: key})
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			state, err := q.ReadInputReceiptReservation(ctx, sqlc.ReadInputReceiptReservationParams{SessionID: session, IdempotencyKey: key})
			if errors.Is(err, pgx.ErrNoRows) {
				result.Status = "absent"
				return nil
			}
			if err != nil {
				return err
			}
			switch state {
			case "pending":
				result.Status = v1.InputReceiptPending
				return nil
			case "failed", "expired", "cancelled":
				result.Status = "failed"
				return nil
			}
			return fmt.Errorf("admitted input reservation has no batch")
		}
		result.Status = "admitted"
		ids := []pgtype.UUID{}
		expected := map[string]string{}
		for _, row := range rows {
			receipt := v1.SessionInputReceipt{InputIndex: int(row.BatchPosition), Sequence: row.Sequence, ItemIDs: []string{}}
			turn := uuidString(row.TurnID)
			if turn != "" {
				receipt.TurnID = &turn
			}
			if row.Kind == "message" {
				updates, err := items.Project(turn, row.Kind, row.Sequence, row.Payload)
				if err != nil {
					return err
				}
				for _, update := range updates {
					id, err := pgunit.ParseID(update.Item.ID)
					if err != nil {
						return err
					}
					ids = append(ids, id)
					expected[update.Item.ID] = turn
					receipt.ItemIDs = append(receipt.ItemIDs, update.Item.ID)
				}
			}
			result.Receipts = append(result.Receipts, receipt)
		}
		if len(ids) > 0 {
			stored, err := q.ReadInputReceiptItems(ctx, sqlc.ReadInputReceiptItemsParams{SessionID: session, ItemIds: ids})
			if err != nil {
				return err
			}
			for _, item := range stored {
				id := uuidString(item.ID)
				if expected[id] == uuidString(item.TurnID) {
					delete(expected, id)
				}
			}
			if len(expected) != 0 {
				return fmt.Errorf("admitted input Item is missing")
			}
		}
		return nil
	})
	return result, err
}
