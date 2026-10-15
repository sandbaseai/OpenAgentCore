package codex

import (
	"bytes"
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

// maxHistoryItemsPerTurn bounds items per Turn while decoding, before the
// per-item json.RawMessage allocations of a hostile frame can accumulate.
const maxHistoryItemsPerTurn = 20000

// UnmarshalJSON decodes items incrementally and fails once a Turn exceeds
// maxHistoryItemsPerTurn, so tiny items cannot force a large transient spike.
func (t *nativeTurn) UnmarshalJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		if err == nil {
			err = errors.New("codex: history turn is not an object")
		}
		return err
	}
	var out nativeTurn
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return err
		}
		key, _ := keyTok.(string)
		switch key {
		case "id":
			err = dec.Decode(&out.ID)
		case "status":
			err = dec.Decode(&out.Status)
		case "itemsView":
			err = dec.Decode(&out.ItemsView)
		case "items":
			err = decodeBoundedItems(dec, &out.Items)
		default:
			var skip json.RawMessage
			err = dec.Decode(&skip)
		}
		if err != nil {
			return err
		}
	}
	*t = out
	return nil
}

// maxHistoryTurns bounds Turns decoded from one response.
const maxHistoryTurns = 10000

// boundedTurns decodes a Turn array incrementally and fails past maxHistoryTurns.
type boundedTurns []nativeTurn

func (b *boundedTurns) UnmarshalJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if tok == nil {
		*b = nil
		return nil
	}
	if tok != json.Delim('[') {
		return errors.New("codex: history turns is not an array")
	}
	var out []nativeTurn
	for dec.More() {
		if len(out) >= maxHistoryTurns {
			return errHistoryTooLarge
		}
		var turn nativeTurn
		if err := dec.Decode(&turn); err != nil {
			return err
		}
		out = append(out, turn)
	}
	*b = out
	return nil
}

func decodeBoundedItems(dec *json.Decoder, items *[]json.RawMessage) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if tok == nil {
		*items = nil
		return nil
	}
	if tok != json.Delim('[') {
		return errors.New("codex: history items is not an array")
	}
	var out []json.RawMessage
	for dec.More() {
		if len(out) >= maxHistoryItemsPerTurn {
			return errHistoryTooLarge
		}
		var item json.RawMessage
		if err := dec.Decode(&item); err != nil {
			return err
		}
		out = append(out, item)
	}
	if _, err := dec.Token(); err != nil {
		return err
	}
	*items = out
	return nil
}

func (r rpcHistoryReader) call(ctx context.Context, method string, args, result any) error {
	raw, err := r.rpc.requestWithTimeout(ctx, method, args, func(frame any) error { return r.rpc.writeFrameContext(ctx, frame) }, 0, nil)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, result)
}

// maxHistoryBytes bounds the retained bytes (items and Turn metadata strings)
// across all pages.
const maxHistoryBytes = 64 * 1024 * 1024

// historyPageLimit keeps each response well below rpcStdoutBufferMax.
const historyPageLimit = 20

// historyItemOverhead approximates the per-item json.RawMessage header and allocation.
const historyItemOverhead = 64

// ReadHistory reads paginated history first so no single RPC frame carries the
// whole thread; the inline thread/read form is only a fallback when listing
// fails.
func (r rpcHistoryReader) ReadHistory(ctx context.Context, threadID string) ([]historyTurn, error) {
	turns, err := r.readPaginated(ctx, threadID)
	if err == nil {
		return turns, nil
	}
	if ctx.Err() != nil || errors.Is(err, errHistoryTooLarge) {
		return nil, err
	}
	var read struct {
		Thread struct {
			ID    string       `json:"id"`
			Turns boundedTurns `json:"turns"`
		} `json:"thread"`
	}
	if rerr := r.call(ctx, "thread/read", map[string]any{"threadId": threadID, "includeTurns": true}, &read); rerr == nil &&
		read.Thread.ID == threadID && inlineHistoryComplete(read.Thread.Turns) {
		if nativeTurnsBytes(read.Thread.Turns) > maxHistoryBytes {
			return nil, errHistoryTooLarge
		}
		return historyTurns(read.Thread.Turns), nil
	}
	return nil, err
}

var errHistoryTooLarge = errors.New("codex: thread history exceeds byte bound")

func (r rpcHistoryReader) readPaginated(ctx context.Context, threadID string) ([]historyTurn, error) {
	var cursor *string
	var turns []nativeTurn
	total := 0
	for range maxHistoryPages {
		var page struct {
			Data       boundedTurns `json:"data"`
			NextCursor *string      `json:"nextCursor"`
		}
		args := map[string]any{"threadId": threadID, "itemsView": "full", "sortDirection": "asc", "limit": historyPageLimit, "cursor": cursor}
		if err := r.call(ctx, "thread/turns/list", args, &page); err != nil {
			return nil, err
		}
		for _, turn := range page.Data {
			if turn.ID == "" || turn.ItemsView != "full" {
				return nil, errors.New("codex: thread history incomplete")
			}
			total += nativeTurnBytes(turn)
			if total > maxHistoryBytes {
				return nil, errHistoryTooLarge
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

// nativeTurnBytes charges the metadata strings plus the slice header and
// allocation each retained item costs, so millions of tiny items cannot stay
// under the byte bound.
func nativeTurnBytes(turn nativeTurn) int {
	total := len(turn.ID) + len(turn.Status) + len(turn.ItemsView)
	for _, item := range turn.Items {
		total += len(item) + historyItemOverhead
	}
	return total
}

func nativeTurnsBytes(turns []nativeTurn) int {
	total := 0
	for _, t := range turns {
		total += nativeTurnBytes(t)
	}
	return total
}

// inlineHistoryComplete accepts thread/read Turns only when each one is
// explicitly marked as carrying its full items; shells or partial items fall
// back to the paginated thread/turns/list read.
func inlineHistoryComplete(turns []nativeTurn) bool {
	if len(turns) == 0 {
		return false
	}
	for _, t := range turns {
		if t.ID == "" || t.ItemsView != "full" {
			return false
		}
	}
	return true
}

func historyTurns(native []nativeTurn) []historyTurn {
	turns := make([]historyTurn, 0, len(native))
	for _, t := range native {
		turns = append(turns, historyTurn{ID: t.ID, Status: t.Status, Items: t.Items})
	}
	return turns
}
