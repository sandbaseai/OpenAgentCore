package v1

type InputReceiptStatus string

const (
	InputReceiptAdmitted InputReceiptStatus = "admitted"
	InputReceiptPending  InputReceiptStatus = "pending"
	InputReceiptFailed   InputReceiptStatus = "failed"
	InputReceiptAbsent   InputReceiptStatus = "absent"
)

// SessionInputReceipts is Core's public extension for durable input admission.
// It reports admission, not Runtime application or Turn completion.
type SessionInputReceipts struct {
	Object    string                `json:"object" binding:"required" enums:"agent.session.input_receipts"`
	SessionID string                `json:"session_id" binding:"required" format:"uuid"`
	Status    InputReceiptStatus    `json:"status" binding:"required"`
	Receipts  []SessionInputReceipt `json:"receipts" binding:"required"`
}

type SessionInputReceipt struct {
	InputIndex int      `json:"input_index" binding:"required" minimum:"0"`
	Sequence   int64    `json:"sequence" binding:"required" minimum:"1"`
	TurnID     *string  `json:"turn_id" binding:"required" extensions:"x-nullable" format:"uuid"`
	ItemIDs    []string `json:"item_ids" binding:"required"`
}
