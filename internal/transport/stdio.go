// Package transport implements the two ways claude-app-server accepts
// clients: NDJSON over stdio, and NDJSON-over-text-frames WebSocket.
package transport

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"sync"

	"github.com/uranix/claude-app-server-go/internal/appserver"
	"github.com/uranix/claude-app-server-go/internal/jsonrpc"
)

// ServeStdio runs the JSON-RPC loop over os.Stdin/os.Stdout until stdin
// closes. It never returns policy decisions (auth, origin, bind address)
// since stdio is inherently local and process-scoped.
func ServeStdio(cfg appserver.Config) error {
	var writeMu sync.Mutex
	out := bufio.NewWriter(os.Stdout)

	send := func(v any) {
		b, err := json.Marshal(v)
		if err != nil {
			return
		}
		writeMu.Lock()
		defer writeMu.Unlock()
		out.Write(b)
		out.WriteByte('\n')
		out.Flush()
	}

	conn := appserver.NewConn(cfg, func(method string, params any) {
		send(jsonrpc.Notif(method, params))
	})
	defer conn.Close()

	return serveNDJSON(os.Stdin, conn, send)
}

// serveNDJSON reads NDJSON requests from r, dispatches them through conn,
// and writes responses via send. Shared by stdio and (per-connection) the
// WebSocket transport's message loop.
func serveNDJSON(r io.Reader, conn *appserver.Conn, send func(v any)) error {
	br := bufio.NewReaderSize(r, 64*1024)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			handleLine(line, conn, send)
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

func handleLine(line []byte, conn *appserver.Conn, send func(v any)) {
	msg, err := jsonrpc.ParseLine(line)
	if err != nil {
		send(jsonrpc.Err(nil, jsonrpc.ErrParse, "parse error", nil))
		return
	}
	if msg == nil {
		return
	}
	if msg.ID == nil {
		// No client-initiated notifications are part of this protocol; drop.
		return
	}

	go func() {
		result, err := conn.HandleRequest(msg.Method, msg.Params)
		if err != nil {
			if exc, ok := err.(*jsonrpc.Exception); ok {
				send(jsonrpc.Err(*msg.ID, exc.Code, exc.Msg, exc.Data))
			} else {
				send(jsonrpc.Err(*msg.ID, jsonrpc.ErrInternal, err.Error(), nil))
			}
			return
		}
		send(jsonrpc.Ok(*msg.ID, result))
	}()
}
