package codex

import (
	"context"
	"encoding/json"
	"errors"
)

// maxHistoryPages bounds how many pages of Turns one recovery reads.
const maxHistoryPages = 100

// rpcHistoryReader reads a thread's Turns over the app-server protocol.
//
// ASSUMPTION (design V2, to be calibrated against a live codex 0.153.4):
// thread/read with includeTurns returns each Turn's items for a root thread, and
// a thread created with historyMode "paginated" instead lists them through
// thread/turns/list. The first form is tried first; the second is the form this
// adapter already relies on for subagent history. When neither yields Turns the
// recovery is abandoned as history_unreadable.
type rpcHistoryReader struct{ rpc *JSONRPCClient }

type nativeTurn struct {
	ID        string            `json:"id"`
	Status    string            `json:"status"`
	Items     []json.RawMessage `json:"items"`
	ItemsView string            `json:"itemsView"`
}

func (r rpcHistoryReader) call(ctx context.Context, method string, args, result any) error {
	raw, err := r.rpc.requestWithTimeout(ctx, method, args, func(frame any) error { return r.rpc.writeFrameContext(ctx, frame) }, 0, nil)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, result)
}

func (r rpcHistoryReader) ReadHistory(ctx context.Context, threadID string) ([]historyTurn, error) {
	var read struct {
		Thread struct {
			ID          string       `json:"id"`
			HistoryMode string       `json:"historyMode"`
			Turns       []nativeTurn `json:"turns"`
		} `json:"thread"`
	}
	err := r.call(ctx, "thread/read", map[string]any{"threadId": threadID, "includeTurns": true}, &read)
	if err == nil && read.Thread.ID == threadID && len(read.Thread.Turns) > 0 {
		return historyTurns(read.Thread.Turns), nil
	}
	if err != nil && ctx.Err() != nil {
		return nil, err
	}
	var cursor *string
	var turns []nativeTurn
	for range maxHistoryPages {
		var page struct {
			Data       []nativeTurn `json:"data"`
			NextCursor *string      `json:"nextCursor"`
		}
		args := map[string]any{"threadId": threadID, "itemsView": "full", "sortDirection": "asc", "limit": 100, "cursor": cursor}
		if err := r.call(ctx, "thread/turns/list", args, &page); err != nil {
			return nil, err
		}
		for _, turn := range page.Data {
			if turn.ID == "" || turn.ItemsView != "full" {
				return nil, errors.New("codex: thread history incomplete")
			}
			turns = append(turns, turn)
		}
		if page.NextCursor == nil {
			if len(turns) == 0 {
				return nil, errors.New("codex: thread history unavailable")
			}
			return historyTurns(turns), nil
		}
		cursor = page.NextCursor
	}
	return nil, errors.New("codex: thread history exceeds read bound")
}

func historyTurns(native []nativeTurn) []historyTurn {
	turns := make([]historyTurn, 0, len(native))
	for _, t := range native {
		turns = append(turns, historyTurn{ID: t.ID, Status: t.Status, Items: t.Items})
	}
	return turns
}
