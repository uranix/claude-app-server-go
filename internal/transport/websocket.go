package transport

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/coder/websocket"

	"github.com/uranix/claude-app-server-go/internal/appserver"
	"github.com/uranix/claude-app-server-go/internal/jsonrpc"
)

// WSOptions configures the WebSocket listener's network exposure and auth.
type WSOptions struct {
	Host           string // interface to bind; "127.0.0.1" unless the user opts out
	Port           int
	AuthKey        string   // required "key" query param, compared in constant time
	AllowedOrigins []string // browser Origin values permitted; none by default
}

const maxMessageSize = 8 << 20 // 8 MiB

// ListenAndServeWS starts the WebSocket listener. Every accepted TCP
// connection completes the WS handshake unconditionally (InsecureSkipVerify
// disables the library's own Origin check); auth key and Origin are enforced
// immediately after with an application close code, mirroring a real
// client's observable behavior: an "open" followed by "close" with that code,
// not a failed handshake.
func ListenAndServeWS(appCfg appserver.Config, opts WSOptions) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		handleWSRequest(w, r, appCfg, opts)
	})

	addr := opts.Host
	if addr == "" {
		addr = "127.0.0.1"
	}
	srv := &http.Server{
		Addr:    addr + ":" + strconv.Itoa(opts.Port),
		Handler: mux,
	}
	return srv.ListenAndServe()
}

func handleWSRequest(w http.ResponseWriter, r *http.Request, appCfg appserver.Config, opts WSOptions) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
	})
	if err != nil {
		return // Accept already wrote the HTTP error response.
	}
	conn.SetReadLimit(maxMessageSize)

	ctx := context.Background()

	if !originAllowed(r.Header.Get("Origin"), opts.AllowedOrigins) {
		_ = conn.Close(websocket.StatusCode(4403), "Origin not allowed")
		return
	}

	clientKey := r.URL.Query().Get("key")
	if !secretEquals(clientKey, opts.AuthKey) {
		_ = conn.Close(websocket.StatusCode(4401), "invalid or missing key")
		return
	}

	runWSConnection(ctx, conn, appCfg)
}

func runWSConnection(ctx context.Context, conn *websocket.Conn, appCfg appserver.Config) {
	appConn := appserver.NewConn(appCfg, func(method string, params any) {
		_ = writeWSMessage(ctx, conn, jsonrpc.Notif(method, params))
	})
	defer appConn.Close()
	defer conn.Close(websocket.StatusNormalClosure, "")

	send := func(v any) { _ = writeWSMessage(ctx, conn, v) }

	for {
		typ, payload, err := conn.Read(ctx)
		if err != nil {
			return
		}
		if typ != websocket.MessageText {
			continue
		}
		for _, line := range splitLines(payload) {
			handleLine(line, appConn, send)
		}
	}
}

func writeWSMessage(ctx context.Context, conn *websocket.Conn, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageText, b)
}

// splitLines splits a WS text frame's payload on newlines, so a client that
// batches several NDJSON messages into one frame is still handled correctly.
func splitLines(payload []byte) [][]byte {
	var lines [][]byte
	start := 0
	for i, b := range payload {
		if b == '\n' {
			lines = append(lines, payload[start:i])
			start = i + 1
		}
	}
	if start < len(payload) {
		lines = append(lines, payload[start:])
	}
	return lines
}

// originAllowed implements claude-app-server's browser-Origin policy:
// non-browser clients (no Origin header at all) are always allowed; browser
// clients are rejected unless their exact Origin is allowlisted. WebSocket
// handshakes are exempt from CORS, so this is the only defense against a
// malicious web page driving the server via a victim's browser.
func originAllowed(origin string, allowed []string) bool {
	if origin == "" {
		return true
	}
	for _, o := range allowed {
		if o == origin {
			return true
		}
	}
	return false
}

// secretEquals is a constant-time comparison so response timing can't leak
// how much of the auth key a guess got right.
func secretEquals(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
