package runtimegateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

func TestTransportFailureClassifiesUnderlyingCause(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		kind string
		code int
	}{
		{"read timeout", &net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded}, "timeout", 0},
		{"abnormal close", &websocket.CloseError{Code: 1006, Text: "unexpected EOF"}, "websocket_close", 1006},
		{"peer text is not classification", &websocket.CloseError{Code: 1008, Text: "secret-peer-text"}, "websocket_close", 1008},
		{"oversized frame", websocket.ErrReadLimit, "frame_too_large", 0},
		{"wrapped EOF", fmt.Errorf("read: %w", io.EOF), "eof", 0},
		{"closed socket", net.ErrClosed, "socket_closed", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kind, code := transportFailure(tc.err)
			if kind != tc.kind || code != tc.code {
				t.Fatalf("got %s/%d, want %s/%d", kind, code, tc.kind, tc.code)
			}
		})
	}
}

func TestTransportFailureLogsWithoutOptionalCallback(t *testing.T) {
	var out bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&out, nil)))
	defer slog.SetDefault(previous)
	s := &Session{DeviceID: "device-fixture"}
	s.logTransportFailure("read", &websocket.CloseError{Code: 1006, Text: "private-peer-text"})
	var row map[string]any
	if err := json.Unmarshal(out.Bytes(), &row); err != nil {
		t.Fatal(err)
	}
	if row["msg"] != "runtime transport failed" || row["operation"] != "read" || row["device_id"] != "device-fixture" || row["close_code"] != float64(1006) || row["error_kind"] != "websocket_close" {
		t.Fatalf("missing diagnostic fields: %v", row)
	}
	if strings.Contains(out.String(), "private-peer-text") {
		t.Fatal("peer text leaked")
	}
}
