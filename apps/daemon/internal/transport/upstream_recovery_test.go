package transport_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/websocket"

	"github.com/MiniMax-AI/OpenAgentCore/apps/daemon/internal/transport"
	"github.com/MiniMax-AI/OpenAgentCore/internal/agentdaemon/proto"
)

// The Runtime always offers the extension; it is active only when Core echoes
// the same selection, and an unknown selection is a permanent protocol error.
func TestDialNegotiatesUpstreamRecovery(t *testing.T) {
	for _, tc := range []struct {
		name     string
		response []string
		selected bool
		wantErr  bool
	}{
		{name: "core selects", response: []string{proto.UpstreamRecoveryV1}, selected: true},
		{name: "older core ignores the offer"},
		{name: "unknown selection", response: []string{"v2"}, wantErr: true},
		{name: "repeated selection", response: []string{proto.UpstreamRecoveryV1, proto.UpstreamRecoveryV1}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			offered := make(chan string, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				offered <- r.Header.Get(proto.UpstreamRecoveryHeader)
				response := http.Header{}
				for _, v := range tc.response {
					response.Add(proto.UpstreamRecoveryHeader, v)
				}
				c, err := (&websocket.Upgrader{}).Upgrade(w, r, response)
				if err == nil {
					defer c.Close()
					_, _, _ = c.ReadMessage()
				}
			}))
			defer srv.Close()
			conn, err := transport.Dial(t.Context(), transport.DialOptions{WSURL: httpToWS(srv.URL), DeviceID: "d", Credential: "fixture", DaemonVersion: proto.Version, UpstreamRecovery: true})
			if got := <-offered; got != proto.UpstreamRecoveryV1 {
				t.Fatalf("offer = %q", got)
			}
			if tc.wantErr {
				if !errors.Is(err, transport.ErrPermanent) {
					t.Fatalf("invalid selection accepted: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if conn.UpstreamRecovery() != tc.selected {
				t.Fatalf("selected = %t", conn.UpstreamRecovery())
			}
		})
	}
}

func TestDialRejectsUnofferedUpstreamRecoverySelection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := (&websocket.Upgrader{}).Upgrade(w, r, http.Header{proto.UpstreamRecoveryHeader: {proto.UpstreamRecoveryV1}})
		if err == nil {
			defer c.Close()
			_, _, _ = c.ReadMessage()
		}
	}))
	defer srv.Close()
	_, err := transport.Dial(t.Context(), transport.DialOptions{WSURL: httpToWS(srv.URL), DeviceID: "d", Credential: "fixture", DaemonVersion: proto.Version})
	if !errors.Is(err, transport.ErrPermanent) {
		t.Fatalf("unoffered selection accepted: %v", err)
	}
}

func TestDialOmitsUpstreamRecoveryWithoutCapableHarness(t *testing.T) {
	offered := make(chan []string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		offered <- r.Header.Values(proto.UpstreamRecoveryHeader)
		c, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err == nil {
			defer c.Close()
			_, _, _ = c.ReadMessage()
		}
	}))
	defer srv.Close()
	conn, err := transport.Dial(t.Context(), transport.DialOptions{WSURL: httpToWS(srv.URL), DeviceID: "d", Credential: "fixture", DaemonVersion: proto.Version})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if got := <-offered; len(got) != 0 {
		t.Fatalf("offer = %v", got)
	}
}
