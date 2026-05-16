package mcp

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// connTransport adapts a net.Conn into an mcp.Transport using newline-
// delimited JSON-RPC, matching the framing the official `custom-transport`
// example uses for stdio. Each accepted Unix-socket connection becomes its
// own ephemeral Transport — the shared *mcp.Server fans across them.
type connTransport struct {
	conn net.Conn
}

// Connect builds the per-session Connection. The MCP server calls this
// exactly once per Transport instance.
func (t *connTransport) Connect(_ context.Context) (mcp.Connection, error) {
	return &connConn{
		conn: t.conn,
		r:    bufio.NewReader(t.conn),
	}, nil
}

// connConn is one MCP connection riding on top of a net.Conn. The mcp
// runtime serializes reads from a single Connection, so we don't need
// extra locking around r/w access.
type connConn struct {
	conn io.Closer
	r    *bufio.Reader
	w    io.Writer
}

// newConnConn lets tests build a Connection over an arbitrary io pair
// (e.g. io.Pipe) without going through a real net.Conn.
func newConnConn(r io.Reader, w io.Writer, closer io.Closer) *connConn {
	return &connConn{
		conn: closer,
		r:    bufio.NewReader(r),
		w:    w,
	}
}

func (c *connConn) Read(_ context.Context) (jsonrpc.Message, error) {
	line, err := c.r.ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	// Strip the trailing newline before handing to the decoder; some
	// implementations are strict about extra whitespace.
	return jsonrpc.DecodeMessage(line[:len(line)-1])
}

func (c *connConn) Write(_ context.Context, msg jsonrpc.Message) error {
	data, err := jsonrpc.EncodeMessage(msg)
	if err != nil {
		return err
	}
	target := c.w
	if target == nil {
		// connTransport-backed connections write straight to the net.Conn.
		// We only need the io.Writer fallback for test plumbing.
		nc, ok := c.conn.(io.Writer)
		if !ok {
			return errors.New("mcp: connection has no writer")
		}
		target = nc
	}
	if _, err := target.Write(data); err != nil {
		return err
	}
	_, err = target.Write([]byte{'\n'})
	return err
}

func (c *connConn) Close() error {
	if c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

// SessionID returns an empty string. Bosun doesn't tag MCP-level sessions
// — each Unix-socket connection is one logical agent session and bosun
// already identifies sessions by their bosun/session-N branch.
func (c *connConn) SessionID() string { return "" }
