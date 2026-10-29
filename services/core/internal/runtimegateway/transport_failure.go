package runtimegateway

import (
	"errors"
	"fmt"
	"io"
	"net"

	obslog "github.com/MiniMax-AI/OpenAgentCore/internal/obs/log"
	"github.com/gorilla/websocket"
)

// Log transport facts without peer-controlled close text or frame contents.
func (s *Session) logTransportFailure(operation string, err error) {
	kind, code := transportFailure(err)
	obslog.Bg().Warn("runtime transport failed", "device_id", s.DeviceID,
		"operation", operation, "error_kind", kind, "close_code", code,
		"error_type", fmt.Sprintf("%T", err))
}

func transportFailure(err error) (string, int) {
	var closeErr *websocket.CloseError
	var netErr net.Error
	switch {
	case errors.As(err, &closeErr):
		return "websocket_close", closeErr.Code
	case errors.As(err, &netErr) && netErr.Timeout():
		return "timeout", 0
	case errors.Is(err, websocket.ErrReadLimit):
		return "frame_too_large", 0
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "eof", 0
	case errors.Is(err, net.ErrClosed):
		return "socket_closed", 0
	default:
		return "io_error", 0
	}
}
