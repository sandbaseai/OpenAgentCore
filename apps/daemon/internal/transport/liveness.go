package transport

import "time"

const (
	peerProbeInterval = 15 * time.Second
	peerIdleTimeout   = 60 * time.Second
)

// A successful local write is not evidence that Core is still reachable through
// a proxy. Require inbound traffic or WebSocket pong replies, even when a Turn
// is busy producing output. Only the reader updates its deadline after setup.
func (c *Conn) configureLiveness(timeout time.Duration) {
	c.peerTimeout = timeout
	_ = c.ws.SetReadDeadline(time.Now().Add(timeout))
	c.ws.SetPongHandler(func(string) error {
		return c.ws.SetReadDeadline(time.Now().Add(timeout))
	})
}
