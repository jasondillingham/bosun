package mcp

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// maxFrameBytes caps how many bytes a single newline-delimited JSON-RPC
// frame may grow to before the daemon refuses it and tears down the
// connection. 16 MiB sits well above realistic frame sizes (tools/list
// output is ~30 KiB; the brief-text cap on bosun_spawn is 256 KiB)
// while preventing the Bughunt-1 F018 unbounded-buffer DoS: a malicious
// caller streaming bytes without a trailing newline could accumulate
// arbitrary RSS in the underlying bufio buffer with no upper bound.
// 8 concurrent attackers were measured pinning 1 GiB+ each pre-fix.
const maxFrameBytes = 16 * 1024 * 1024

// errFrameTooLarge signals a peer exceeded maxFrameBytes without a
// terminating newline. Once the framing contract breaks, the buffer
// state is unrecoverable; the caller (the MCP SDK runtime) closes the
// connection on this error.
var errFrameTooLarge = fmt.Errorf("mcp: frame exceeded %d bytes without newline (Bughunt-1 F018)", maxFrameBytes)

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
	line, err := readLineCapped(c.r, maxFrameBytes)
	if err != nil {
		return nil, err
	}
	return jsonrpc.DecodeMessage(line)
}

// readLineCapped reads bytes up to and including the next newline,
// returning the line WITHOUT the terminating newline. Refuses frames
// that exceed max bytes — defends against bufio.Reader.ReadBytes('\n')'s
// unbounded growth when a peer streams bytes without ever sending a
// delimiter (Bughunt-1 F018).
//
// On read error before a newline is found, returns nil and the error
// (matching the prior ReadBytes behavior — incomplete framing is not
// a recoverable state).
func readLineCapped(r *bufio.Reader, max int) ([]byte, error) {
	buf := make([]byte, 0, 1024)
	for {
		b, err := r.ReadByte()
		if err != nil {
			return nil, err
		}
		if b == '\n' {
			return buf, nil
		}
		if len(buf) >= max {
			return nil, errFrameTooLarge
		}
		buf = append(buf, b)
	}
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
