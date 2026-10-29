package transport

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestLivenessDetectsSilentPeerAndAllowsResponsiveIdlePeer(t *testing.T) {
	for _, responsive := range []bool{false, true} {
		name := "silent"
		if responsive {
			name = "responsive"
		}
		t.Run(name, func(t *testing.T) {
			stop := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ws, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer ws.Close()
				if !responsive {
					<-stop
					return
				}
				for {
					if _, _, err := ws.ReadMessage(); err != nil {
						return
					}
				}
			}))
			defer srv.Close()
			defer close(stop)
			c, err := dial(t.Context(), DialOptions{WSURL: "ws" + strings.TrimPrefix(srv.URL, "http"), DeviceID: "test", Credential: "test", DaemonVersion: "0.22.0"}, 20*time.Millisecond, 200*time.Millisecond)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			deadline := time.NewTimer(800 * time.Millisecond)
			defer deadline.Stop()
			for {
				select {
				case <-c.Done():
					if responsive {
						t.Fatalf("healthy idle connection closed: %v", c.Err())
					}
					var ne net.Error
					if !errors.As(c.Err(), &ne) || !ne.Timeout() {
						t.Fatalf("expected timeout, got %v", c.Err())
					}
					return
				case <-deadline.C:
					if !responsive {
						t.Fatal("silent peer remained connected")
					}
					return
				}
			}
		})
	}
}

func TestLivenessBoundsBlockedInboundDispatch(t *testing.T) {
	stop := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		for i := 0; i < 65; i++ {
			if err := ws.WriteMessage(websocket.TextMessage, []byte(`{"type":"heartbeat","id":"fixture","payload":{}}`)); err != nil {
				return
			}
		}
		<-stop
	}))
	defer srv.Close()
	defer close(stop)
	c, err := dial(t.Context(), DialOptions{WSURL: "ws" + strings.TrimPrefix(srv.URL, "http"), DeviceID: "test", Credential: "test", DaemonVersion: "0.22.0"}, 20*time.Millisecond, 200*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	select {
	case <-c.Done():
		if c.Err() == nil || !strings.Contains(c.Err().Error(), "inbound dispatch timeout") {
			t.Fatalf("unexpected failure: %v", c.Err())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("full receive queue prevented connection cleanup")
	}
}
