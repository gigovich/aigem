package chat

import (
	"net"
	"sync"
	"time"

	"github.com/gobwas/ws"
)

// fakeConn is a net.Conn that records what was written to it, so the frame pump
// can be tested without a listener, a client, or a timeout to wait out.
type fakeConn struct {
	mu      sync.Mutex
	written []byte
}

func newFakeConn() *fakeConn { return &fakeConn{} }

// texts returns the websocket text frames written so far.
func (c *fakeConn) texts() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for rest := c.written; len(rest) >= 2; {
		// Server frames are unmasked, and nothing here writes a payload long
		// enough for the 64-bit length form.
		opcode := ws.OpCode(rest[0] & 0x0f)
		n := int(rest[1] & 0x7f)
		rest = rest[2:]
		if n == 126 {
			if len(rest) < 2 {
				return out
			}
			n = int(rest[0])<<8 | int(rest[1])
			rest = rest[2:]
		}
		if n > len(rest) {
			return out
		}
		if opcode == ws.OpText {
			out = append(out, string(rest[:n]))
		}
		rest = rest[n:]
	}
	return out
}

func (c *fakeConn) Write(b []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.written = append(c.written, b...)
	return len(b), nil
}

func (c *fakeConn) Read([]byte) (int, error)         { return 0, net.ErrClosed }
func (c *fakeConn) Close() error                     { return nil }
func (c *fakeConn) LocalAddr() net.Addr              { return fakeAddr{} }
func (c *fakeConn) RemoteAddr() net.Addr             { return fakeAddr{} }
func (c *fakeConn) SetDeadline(time.Time) error      { return nil }
func (c *fakeConn) SetReadDeadline(time.Time) error  { return nil }
func (c *fakeConn) SetWriteDeadline(time.Time) error { return nil }

type fakeAddr struct{}

func (fakeAddr) Network() string { return "fake" }
func (fakeAddr) String() string  { return "fake" }
