package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

func main() {
	var (
		api      = flag.String("api", envOrDefault("PGTUNNEL_API", "http://localhost:8000"), "API base URL")
		project  = flag.String("project", "", "project id or name")
		listen   = flag.String("listen", "127.0.0.1:15432", "local listen address")
		token    = flag.String("token", os.Getenv("PGTUNNEL_TOKEN"), "Bearer token; defaults to PGTUNNEL_TOKEN")
		devSub   = flag.String("dev-sub", envOrDefault("PGTUNNEL_DEV_SUB", "dev-user"), "X-Dev-Sub for AUTH_DISABLED local testing")
		insecure = flag.Bool("insecure-unauthenticated-local-test", false, "omit Authorization header for local test only")
	)
	flag.Parse()

	if *project == "" {
		log.Fatal("-project is required")
	}
	if *token == "" && !*insecure {
		log.Fatal("-token or PGTUNNEL_TOKEN is required")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatal(err)
	}
	defer ln.Close()
	log.Printf("listening on %s -> %s/projects/%s/tunnel", *listen, strings.TrimRight(*api, "/"), *project)

	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("accept: %v", err)
			continue
		}
		go func() {
			if err := handleConn(ctx, conn, *api, *project, *token, *devSub, *insecure); err != nil {
				log.Printf("connection closed: %v", err)
			}
		}()
	}
}

func handleConn(ctx context.Context, local net.Conn, api, project, token, devSub string, insecure bool) error {
	defer local.Close()

	wsURL, err := tunnelURL(api, project)
	if err != nil {
		return err
	}
	headers := http.Header{}
	if !insecure {
		headers.Set("Authorization", "Bearer "+token)
	} else {
		headers.Set("X-Dev-Sub", devSub)
	}

	dialer := websocket.Dialer{HandshakeTimeout: 15 * time.Second}
	ws, _, err := dialer.DialContext(ctx, wsURL, headers)
	if err != nil {
		return err
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
			messageType, r, err := ws.NextReader()
			if err != nil {
				closeBoth(err)
				return
			}
			if messageType != websocket.BinaryMessage {
				continue
			}
			if _, err := io.Copy(tcp, r); err != nil {
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

func tunnelURL(api, project string) (string, error) {
	base := strings.TrimRight(api, "/")
	switch {
	case strings.HasPrefix(base, "https://"):
		base = "wss://" + strings.TrimPrefix(base, "https://")
	case strings.HasPrefix(base, "http://"):
		base = "ws://" + strings.TrimPrefix(base, "http://")
	default:
		return "", fmt.Errorf("api must start with http:// or https://")
	}
	return base + "/projects/" + project + "/tunnel", nil
}

func envOrDefault(key, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	return value
}
