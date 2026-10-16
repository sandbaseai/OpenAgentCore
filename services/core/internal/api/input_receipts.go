package api

import (
	"github.com/go-chi/chi/v5"
	"net/http"
)

// getInputReceipts reads the committed identity of a submitted input batch.
// @Summary Get Session input receipts
// @Description Project key. Core extension: exact Idempotency-Key batch admission, not execution completion. A missing batch can still be in flight.
// @Tags Session input receipts
// @Produce json
// @Param session_id path string true "Session ID"
// @Param Idempotency-Key header string true "Exact input batch key" maxlength(128)
// @Success 200 {object} v1.SessionInputReceipts
// @Success 202 {object} v1.SessionInputReceipts
// @Failure 400 {object} v1.ErrorResponse
// @Failure 404 {object} v1.ErrorResponse
// @Router /v1/agents/sessions/{session_id}/input-receipts [get]
func (h *Handler) getInputReceipts(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	result, err := h.SessionsReader.GetInputReceipts(r.Context(), tenantID(r), chi.URLParam(r, "session_id"), r.Header.Get("Idempotency-Key"))
	if err != nil {
		writeSessionsError(w, r, err)
		return
	}
	status := http.StatusOK
	if result.Status == "pending" {
		status = http.StatusAccepted
	}
	writeJSON(w, status, result)
}
