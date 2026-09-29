// Package ws implements the server side of RFC 6455 WebSockets using only
// the standard library: HTTP upgrade via net/http.Hijacker, and a hand-rolled
// frame codec. It implements just enough of the spec for a JSON-over-text-
// frames NDJSON protocol: text/binary frames, continuation, ping/pong, close.
package ws

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
)

const acceptGUID = "258EAFA5-E914-47DA-95CA-5AB0DC85B11C"

// Opcodes per RFC 6455 section 5.2.
const (
	OpContinuation = 0x0
	OpText         = 0x1
	OpBinary       = 0x2
	OpClose        = 0x8
	OpPing         = 0x9
	OpPong         = 0xA
)

// MaxMessageSize bounds a single (possibly reassembled) message. Frames that
// would push a message past this are rejected and the connection is closed
// with code 1009 (message too big).
const MaxMessageSize = 8 << 20 // 8 MiB

var (
	ErrMessageTooBig = errors.New("ws: message exceeds max size")
	ErrClosed        = errors.New("ws: connection closed")
)

// Conn is an upgraded WebSocket connection.
type Conn struct {
	rw     net.Conn
	br     *bufio.Reader
	bw     *bufio.Writer
	closed bool
}

// AcceptKey computes the Sec-WebSocket-Accept value for a given client key,
// per RFC 6455 section 4.2.2.
func AcceptKey(clientKey string) string {
	h := sha1.New()
	h.Write([]byte(clientKey))
	h.Write([]byte(acceptGUID))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// Upgrade validates the WebSocket handshake headers, hijacks the underlying
// TCP connection, and completes the handshake. Upgrade only implements the
// protocol; it has no opinion on auth keys or Origin allowlisting -- callers
// apply that policy after Upgrade returns, closing with an application close
// code (e.g. 4401, 4403) rather than failing the HTTP upgrade itself. This
// matches how a real browser WebSocket client observes rejection: a normal
// "open" followed immediately by "close" with the given code, not a failed
// handshake.
func Upgrade(w http.ResponseWriter, r *http.Request) (*Conn, error) {
	if !strings.EqualFold(r.Header.Get("Connection"), "Upgrade") && !headerContainsToken(r.Header.Get("Connection"), "upgrade") {
		return nil, errors.New("ws: missing Connection: Upgrade")
	}
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return nil, errors.New("ws: missing Upgrade: websocket")
	}
	if r.Header.Get("Sec-WebSocket-Version") != "13" {
		return nil, errors.New("ws: unsupported Sec-WebSocket-Version")
	}
	clientKey := r.Header.Get("Sec-WebSocket-Key")
	if clientKey == "" {
		return nil, errors.New("ws: missing Sec-WebSocket-Key")
	}

	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil, errors.New("ws: response writer does not support hijacking")
	}
	conn, buf, err := hj.Hijack()
	if err != nil {
		return nil, err
	}

	accept := AcceptKey(clientKey)
	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + accept + "\r\n\r\n"
	if _, err := buf.Writer.WriteString(resp); err != nil {
		conn.Close()
		return nil, err
	}
	if err := buf.Writer.Flush(); err != nil {
		conn.Close()
		return nil, err
	}

	return &Conn{rw: conn, br: buf.Reader, bw: bufio.NewWriter(conn)}, nil
}

func headerContainsToken(header, token string) bool {
	for _, part := range strings.Split(header, ",") {
		if strings.EqualFold(strings.TrimSpace(part), token) {
			return true
		}
	}
	return false
}

type frameHeader struct {
	fin    bool
	opcode int
	masked bool
	length uint64
	mask   [4]byte
}

func (c *Conn) readFrameHeader() (frameHeader, error) {
	var fh frameHeader
	b, err := readN(c.br, 2)
	if err != nil {
		return fh, err
	}
	fh.fin = b[0]&0x80 != 0
	fh.opcode = int(b[0] & 0x0F)
	fh.masked = b[1]&0x80 != 0
	length := uint64(b[1] & 0x7F)

	switch length {
	case 126:
		ext, err := readN(c.br, 2)
		if err != nil {
			return fh, err
		}
		length = uint64(ext[0])<<8 | uint64(ext[1])
	case 127:
		ext, err := readN(c.br, 8)
		if err != nil {
			return fh, err
		}
		length = 0
		for _, x := range ext {
			length = length<<8 | uint64(x)
		}
	}
	fh.length = length

	if fh.masked {
		maskBytes, err := readN(c.br, 4)
		if err != nil {
			return fh, err
		}
		copy(fh.mask[:], maskBytes)
	}
	return fh, nil
}

func readN(r io.Reader, n int) ([]byte, error) {
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

func unmask(payload []byte, mask [4]byte) {
	for i := range payload {
		payload[i] ^= mask[i%4]
	}
}

// ReadMessage reads one logical message, reassembling continuation frames
// and transparently answering pings. It returns the opcode of the first
// frame (OpText or OpBinary) and the concatenated, unmasked payload.
func (c *Conn) ReadMessage() (int, []byte, error) {
	var (
		msgOpcode int
		payload   []byte
		started   bool
	)
	for {
		fh, err := c.readFrameHeader()
		if err != nil {
			return 0, nil, err
		}
		if !fh.masked {
			return 0, nil, errors.New("ws: client frame not masked")
		}
		if fh.length > MaxMessageSize {
			_ = c.writeControl(OpClose, closePayload(1009, "message too big"))
			return 0, nil, ErrMessageTooBig
		}

		data, err := readN(c.br, int(fh.length))
		if err != nil {
			return 0, nil, err
		}
		unmask(data, fh.mask)

		switch fh.opcode {
		case OpPing:
			if err := c.writeControl(OpPong, data); err != nil {
				return 0, nil, err
			}
			continue
		case OpPong:
			continue
		case OpClose:
			_ = c.writeControl(OpClose, data)
			return 0, nil, ErrClosed
		case OpContinuation:
			if !started {
				return 0, nil, errors.New("ws: continuation without start frame")
			}
		case OpText, OpBinary:
			if started {
				return 0, nil, errors.New("ws: new message before previous finished")
			}
			msgOpcode = fh.opcode
			started = true
		default:
			return 0, nil, fmt.Errorf("ws: unsupported opcode %d", fh.opcode)
		}

		if len(payload)+len(data) > MaxMessageSize {
			_ = c.writeControl(OpClose, closePayload(1009, "message too big"))
			return 0, nil, ErrMessageTooBig
		}
		payload = append(payload, data...)

		if fh.fin {
			return msgOpcode, payload, nil
		}
	}
}

// WriteMessage sends a single unmasked frame (server frames are never
// masked, per spec) carrying the whole payload as one fragment.
func (c *Conn) WriteMessage(opcode int, payload []byte) error {
	return c.writeFrame(true, opcode, payload)
}

func (c *Conn) writeControl(opcode int, payload []byte) error {
	return c.writeFrame(true, opcode, payload)
}

func (c *Conn) writeFrame(fin bool, opcode int, payload []byte) error {
	var header []byte
	b0 := byte(opcode)
	if fin {
		b0 |= 0x80
	}
	header = append(header, b0)

	n := len(payload)
	switch {
	case n < 126:
		header = append(header, byte(n))
	case n <= 0xFFFF:
		header = append(header, 126, byte(n>>8), byte(n))
	default:
		header = append(header, 127,
			byte(n>>56), byte(n>>48), byte(n>>40), byte(n>>32),
			byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
	}

	if _, err := c.bw.Write(header); err != nil {
		return err
	}
	if _, err := c.bw.Write(payload); err != nil {
		return err
	}
	return c.bw.Flush()
}

func closePayload(code int, reason string) []byte {
	p := make([]byte, 2+len(reason))
	p[0] = byte(code >> 8)
	p[1] = byte(code)
	copy(p[2:], reason)
	return p
}

// Close sends a close frame with the given status code and reason, then
// closes the underlying TCP connection.
func (c *Conn) Close(code int, reason string) error {
	if c.closed {
		return nil
	}
	c.closed = true
	_ = c.writeControl(OpClose, closePayload(code, reason))
	return c.rw.Close()
}
