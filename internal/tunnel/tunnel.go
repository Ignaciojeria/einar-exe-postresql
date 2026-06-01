package tunnel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type Options struct {
	APIBaseURL   string
	Project      string
	Token        string
	DevSub       string
	InsecureAuth bool

	HandshakeTimeout time.Duration
}

func Serve(ctx context.Context, listenAddr string, options Options, logf func(format string, args ...any)) error {
	if options.HandshakeTimeout <= 0 {
		options.HandshakeTimeout = 15 * time.Second
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}

	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return err
	}
	defer ln.Close()

	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			logf("accept: %v", err)
			continue
		}

		go func(c net.Conn) {
			if err := handleConn(ctx, c, options); err != nil {
				logf("connection closed: %v", err)
			}
		}(conn)
	}
}

func TunnelURL(api, project string) (string, error) {
	base := strings.TrimRight(api, "/")
	switch {
	case strings.HasPrefix(base, "https://"):
		base = "wss://" + strings.TrimPrefix(base, "https://")
	case strings.HasPrefix(base, "http://"):
		base = "ws://" + strings.TrimPrefix(base, "http://")
	default:
		return "", fmt.Errorf("api must start with http:// or https://")
	}
	return base + "/projects/" + url.PathEscape(project) + "/tunnel", nil
}

func formatHandshakeError(err error, resp *http.Response) error {
	if resp == nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	text := strings.TrimSpace(string(body))
	if text == "" {
		return fmt.Errorf("websocket handshake failed: HTTP %d %s: %w", resp.StatusCode, resp.Status, err)
	}
	return fmt.Errorf("websocket handshake failed: HTTP %d %s: %s: %w", resp.StatusCode, resp.Status, text, err)
}

func handleConn(ctx context.Context, local net.Conn, options Options) error {
	defer local.Close()

	wsURL, err := TunnelURL(options.APIBaseURL, options.Project)
	if err != nil {
		return err
	}

	headers := http.Header{}
	if options.InsecureAuth {
		headers.Set("X-Dev-Sub", options.DevSub)
	} else {
		headers.Set("Authorization", "Bearer "+options.Token)
	}

	dialer := websocket.Dialer{HandshakeTimeout: options.HandshakeTimeout}
	ws, resp, err := dialer.DialContext(ctx, wsURL, headers)
	if err != nil {
		return formatHandshakeError(err, resp)
	}
	defer ws.Close()

	return proxyTCPToWebSocket(ctx, local, ws)
}

func proxyTCPToWebSocket(ctx context.Context, tcp net.Conn, ws *websocket.Conn) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	var once sync.Once
	var firstErr error
	closeBoth := func(err error) {
		once.Do(func() {
			firstErr = err
			cancel()
			_ = tcp.Close()
			_ = ws.Close()
		})
	}

	wg.Add(2)

	go func() {
		defer wg.Done()
		buf := make([]byte, 32*1024)
		for {
			n, err := tcp.Read(buf)
			if n > 0 {
				if writeErr := ws.WriteMessage(websocket.BinaryMessage, buf[:n]); writeErr != nil {
					closeBoth(writeErr)
					return
				}
			}
			if err != nil {
				if errors.Is(err, net.ErrClosed) || errors.Is(err, io.EOF) {
					closeBoth(nil)
				} else {
					closeBoth(err)
				}
				return
			}
		}
	}()

	go func() {
		defer wg.Done()
		for {
			messageType, reader, err := ws.NextReader()
			if err != nil {
				closeBoth(err)
				return
			}
			if messageType != websocket.BinaryMessage {
				continue
			}
			if _, err := io.Copy(tcp, reader); err != nil {
				closeBoth(err)
				return
			}
		}
	}()

	wg.Wait()
	if firstErr != nil && ctx.Err() == nil {
		return firstErr
	}
	return firstErr
}
