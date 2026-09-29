// Command claude-app-server is a JSON-RPC 2.0 server wrapping the Claude
// Code CLI, exposed over stdio or WebSocket. See README.md.
package main

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/uranix/claude-app-server-go/internal/appserver"
	"github.com/uranix/claude-app-server-go/internal/transport"
)

const defaultPort = 3284

type parsedArgs struct {
	subcommand             string // "" (stdio) or "start" (websocket)
	host                   string
	port                   int
	allowedOrigins         []string
	allowBypassPermissions bool
	debug                  bool
	idleTimeout            time.Duration
	permissionTimeout      time.Duration
	keyFile                string // fixed auth key kept in this file (created if missing)
	help                   bool
}

func parseArgs(argv []string) (parsedArgs, error) {
	a := parsedArgs{host: "127.0.0.1", port: defaultPort, idleTimeout: 10 * time.Minute, permissionTimeout: 5 * time.Minute}

	i := 0
	if len(argv) > 0 && argv[0] == "start" {
		a.subcommand = "start"
		i = 1
	}

	for ; i < len(argv); i++ {
		arg := argv[i]
		next := func() (string, error) {
			i++
			if i >= len(argv) {
				return "", fmt.Errorf("%s requires a value", arg)
			}
			return argv[i], nil
		}

		switch arg {
		case "--host":
			v, err := next()
			if err != nil {
				return a, err
			}
			a.host = v
		case "--port":
			v, err := next()
			if err != nil {
				return a, err
			}
			p, err := strconv.Atoi(v)
			if err != nil {
				return a, fmt.Errorf("--port: invalid number %q", v)
			}
			a.port = p
		case "--allow-origin":
			v, err := next()
			if err != nil {
				return a, err
			}
			a.allowedOrigins = append(a.allowedOrigins, v)
		case "--transport":
			v, err := next()
			if err != nil {
				return a, err
			}
			if v == "ws" {
				a.subcommand = "start"
			}
		case "--dangerously-allow-bypass-permissions":
			a.allowBypassPermissions = true
		case "--debug":
			a.debug = true
		case "--idle-timeout":
			v, err := next()
			if err != nil {
				return a, err
			}
			secs, err := strconv.Atoi(v)
			if err != nil {
				return a, fmt.Errorf("--idle-timeout: invalid number %q", v)
			}
			a.idleTimeout = time.Duration(secs) * time.Second
		case "--permission-timeout":
			v, err := next()
			if err != nil {
				return a, err
			}
			secs, err := strconv.Atoi(v)
			if err != nil || secs <= 0 {
				return a, fmt.Errorf("--permission-timeout: want a positive number of seconds, got %q", v)
			}
			a.permissionTimeout = time.Duration(secs) * time.Second
		case "--key-file":
			v, err := next()
			if err != nil {
				return a, err
			}
			a.keyFile = v
		case "--help", "-h":
			a.help = true
		default:
			return a, fmt.Errorf("unknown flag: %s", arg)
		}
	}
	return a, nil
}

const helpText = `claude-app-server-go - JSON-RPC 2.0 server wrapping the Claude Code CLI

Usage:
  claude-app-server                         stdio transport (safest; no listener)
  claude-app-server start [flags]           WebSocket transport
  claude-app-server --transport ws [flags]  WebSocket transport, no banner

Flags:
  --host <addr>                            bind address (default 127.0.0.1)
  --port <n>                               listen port (default 3284)
  --allow-origin <origin>                  allow this exact browser Origin (repeatable)
  --idle-timeout <seconds>                 idle time before a thread's process is reaped (default 600)
  --permission-timeout <seconds>           auto-deny an unanswered permission prompt after this long (default 300)
  --key-file <path>                        keep the auth key in this file (created with mode 600 if
                                           missing), so it survives restarts; the banner then omits it
  --dangerously-allow-bypass-permissions   allow clients to request bypassPermissions
  --debug                                  verbose logging to stderr
  --help                                   show this help

Security:
  Anyone who can reach the port and present the auth key can run commands as
  you, in any directory. The connect URL is effectively a root password.
    - Default bind is loopback only (127.0.0.1). Pass --host 0.0.0.0 to expose it.
    - Browser-originated connections are rejected unless allowlisted with --allow-origin.
    - bypassPermissions is refused unless --dangerously-allow-bypass-permissions is set.
    - There is no TLS: this binary only speaks plain ws://. Put it behind an
      SSH tunnel or a TLS-terminating reverse proxy if you must cross a network.
`

func generateAuthKey() (string, error) {
	b := make([]byte, 16) // 128 bits
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func isLoopbackHost(host string) bool {
	switch host {
	case "127.0.0.1", "localhost", "::1":
		return true
	default:
		return false
	}
}

func findClaudePath() (string, error) {
	if p := os.Getenv("CLAUDE_PATH"); p != "" {
		return p, nil
	}
	p, err := exec.LookPath("claude")
	if err != nil {
		return "", errors.New("claude CLI not found on PATH; install it or set CLAUDE_PATH")
	}
	return p, nil
}

// loadOrCreateKey returns the auth key stored in path, creating the file (mode
// 600, never overwriting) when it does not exist yet. A key file readable by
// group or others is refused: the key is effectively a password for code execution.
func loadOrCreateKey(path string) (string, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		key, err := generateAuthKey()
		if err != nil {
			return "", err
		}
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return "", fmt.Errorf("create key file: %w", err)
		}
		if _, err := f.WriteString(key + "\n"); err != nil {
			f.Close()
			return "", err
		}
		return key, f.Close()
	}
	if err != nil {
		return "", fmt.Errorf("read key file: %w", err)
	}
	st, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if st.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("key file %s must not be accessible by group or others (chmod 600)", path)
	}
	key := strings.TrimSpace(string(b))
	if len(key) < 16 || strings.Trim(key, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-") != "" {
		return "", fmt.Errorf("key file %s: want at least 16 characters of [A-Za-z0-9_-]", path)
	}
	return key, nil
}

func printStartBanner(host string, port int, authKey string) {
	loopback := isLoopbackHost(host)
	fmt.Fprintln(os.Stderr, "  claude-app-server-go  ·  WebSocket")
	fmt.Fprintln(os.Stderr, "  ---------------------------------")
	fmt.Fprintf(os.Stderr, "  Bound to: %s:%d\n", host, port)

	connectHost := host
	if loopback {
		connectHost = "localhost"
	}
	fmt.Fprintf(os.Stderr, "  Connect:  ws://%s:%d?key=%s\n\n", connectHost, port, authKey)

	if loopback {
		fmt.Fprintln(os.Stderr, "  Loopback only: other devices cannot reach this server.")
		fmt.Fprintln(os.Stderr, "  To pair a device on your LAN, restart with --host 0.0.0.0")
		fmt.Fprintln(os.Stderr, "  (or forward the port over SSH, which is safer).")
	} else {
		fmt.Fprintln(os.Stderr, "  WARNING: reachable from the network. Traffic is NOT encrypted")
		fmt.Fprintln(os.Stderr, "  (plain ws://) and the key above grants full code execution.")
		fmt.Fprintln(os.Stderr, "  Prefer an SSH tunnel over exposing this directly.")
	}
	fmt.Fprintln(os.Stderr)
}

func run() error {
	args, err := parseArgs(os.Args[1:])
	if err != nil {
		return err
	}
	if args.help {
		fmt.Fprint(os.Stderr, helpText)
		return nil
	}

	claudePath, err := findClaudePath()
	if err != nil {
		return err
	}

	appCfg := appserver.DefaultConfig(claudePath)
	appCfg.AllowBypassPermissions = args.allowBypassPermissions
	appCfg.IdleTimeout = args.idleTimeout
	appCfg.PermissionTimeout = args.permissionTimeout

	if args.subcommand != "start" {
		return transport.ServeStdio(appCfg)
	}

	var authKey string
	if args.keyFile != "" {
		authKey, err = loadOrCreateKey(args.keyFile)
	} else {
		authKey, err = generateAuthKey()
	}
	if err != nil {
		return err
	}
	shown := authKey
	if args.keyFile != "" {
		shown = "<contents of " + args.keyFile + ">" // keep the key out of logs and journals
	}
	printStartBanner(args.host, args.port, shown)

	opts := transport.WSOptions{
		Host:           args.host,
		Port:           args.port,
		AuthKey:        authKey,
		AllowedOrigins: args.allowedOrigins,
	}
	fmt.Fprintf(os.Stderr, "listening on %s:%d\n", args.host, args.port)
	return transport.ListenAndServeWS(appCfg, opts)
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
