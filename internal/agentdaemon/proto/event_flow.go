package proto

import (
	"encoding/json"
	"errors"
)

// Execution event credit belongs to one Run on one connection. Acknowledgement
// means consumption by Core, not durable commit or a native execution receipt.
const (
	ExecutionFlowHeader       = "X-Oac-Execution-Flow"
	ExecutionFlowCreditV1     = "credit-v1"
	TypeExecutionEventsAck    = "execution_events_ack"
	TypeExecutionEventsStop   = "execution_events_stop"
	ExecutionEventWindow      = 256
	ExecutionEventWindowBytes = MaxFrameBytes + 64
)

type ExecutionEventsAckPayload struct {
	Count uint64 `json:"count"`
	Bytes uint64 `json:"bytes"`
}

func IsExecutionEvent(kind string) bool {
	switch kind {
	case TypeDelta, TypeOutputMessage, TypeReasoning, TypeToolCall, TypeCommandOutput,
		TypeUsage, TypeError, TypeDone, TypePromptSteerAck, TypeFunctionCall,
		TypeSubagentIdentity, TypeSubagentLifecycle, TypeSubagentTurn, TypeSubagentItem, TypeSubagentCoordination:
		return true
	}
	return false
}

// ExecutionEventBytes accounts for the normalized wire representation. RawMessage
// whitespace and HTML escaping can change during transport marshaling.
func ExecutionEventBytes(env Envelope) uint64 {
	raw, err := json.Marshal(env)
	if err != nil {
		return ExecutionEventWindowBytes + 1
	}
	return uint64(len(raw) + 64)
}

// NegotiateExecutionFlow validates one optional upgrade offer/selection.
// Absence explicitly selects the base protocol without event credit.
func NegotiateExecutionFlow(values []string) (bool, error) {
	if len(values) == 0 {
		return false, nil
	}
	if len(values) == 1 && values[0] == ExecutionFlowCreditV1 {
		return true, nil
	}
	return false, errors.New("unsupported execution flow selection")
}
