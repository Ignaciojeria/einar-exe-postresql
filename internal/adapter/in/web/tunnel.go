package in

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	postgresout "postgresql/internal/adapter/out/postgres"
	"postgresql/internal/shared/server"
	"postgresql/internal/shared/server/middleware"

	"github.com/Ignaciojeria/ioc"
	"github.com/go-fuego/fuego"
	"github.com/gorilla/websocket"
)

var _ = ioc.Register(tunnelHandler)

const (
	postgresTargetAddr = "127.0.0.1:5432"
	tunnelMaxLifetime  = 8 * time.Hour
)

var tunnelUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func tunnelHandler(s *server.Server, provisioner *postgresout.Provisioner) {
	fuego.Get(s.Server, "/projects/{project}/tunnel", func(c fuego.ContextNoBody) (any, error) {
		project := c.PathParam("project")
		return nil, handleTunnel(c, project, provisioner)
	})
}

func handleTunnel(c fuego.ContextNoBody, projectIDOrName string, provisioner *postgresout.Provisioner) error {
	ctx := c.Context()
	claims, ok := middleware.JWTClaimsFromContext(ctx)
	if !ok {
		return fuego.HTTPError{Status: http.StatusUnauthorized, Detail: "unauthorized"}
	}

	sub, ok := claims["sub"].(string)
	if !ok || sub == "" {
		return fuego.HTTPError{Status: http.StatusUnauthorized, Detail: "missing sub claim"}
	}

	if projectIDOrName == "" {
		return fuego.HTTPError{Status: http.StatusBadRequest, Detail: "missing project"}
	}

	lookupCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	project, found, err := provisioner.FindProjectForOwner(lookupCtx, sub, projectIDOrName)
	if err != nil {
		slog.Error("cannot authorize tunnel", "err", err, "sub", sub, "project", projectIDOrName)
		return fuego.HTTPError{Status: http.StatusInternalServerError, Detail: "cannot authorize tunnel"}
	}
	if !found {
		return fuego.HTTPError{Status: http.StatusNotFound, Detail: "project not found"}
	}

	req := c.Request()
	w := c.Response()
	ws, err := tunnelUpgrader.Upgrade(w, req, nil)
	if err != nil {
		return nil
	}
	defer ws.Close()

	pgConn, err := net.DialTimeout("tcp", postgresTargetAddr, 10*time.Second)
	if err != nil {
		slog.Error("cannot connect tunnel target", "err", err, "project_id", project.ID)
		_ = ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseTryAgainLater, "database unavailable"), time.Now().Add(time.Second))
		return nil
	}
	defer pgConn.Close()

	_ = pgConn.SetDeadline(time.Now().Add(tunnelMaxLifetime))
	slog.Info("postgres tunnel connected", "sub", sub, "project_id", project.ID, "project", project.Name)
	defer slog.Info("postgres tunnel disconnected", "sub", sub, "project_id", project.ID, "project", project.Name)

	return proxyWebSocketToTCP(ctx, ws, pgConn)
}

func proxyWebSocketToTCP(ctx context.Context, ws *websocket.Conn, tcp net.Conn) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	var once sync.Once
	closeBoth := func() {
		once.Do(func() {
			cancel()
			_ = tcp.Close()
			_ = ws.Close()
		})
	}

	wg.Add(2)

	go func() {
		defer wg.Done()
		defer closeBoth()
		buf := make([]byte, 32*1024)
		for {
			n, err := tcp.Read(buf)
			if n > 0 {
				if writeErr := ws.WriteMessage(websocket.BinaryMessage, buf[:n]); writeErr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	go func() {
		defer wg.Done()
		defer closeBoth()
		for {
			messageType, r, err := ws.NextReader()
			if err != nil {
				return
			}
			if messageType != websocket.BinaryMessage {
				continue
			}
			if _, err := io.Copy(tcp, r); err != nil {
				return
			}
		}
	}()

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-ctx.Done():
		closeBoth()
		<-done
		if errors.Is(ctx.Err(), context.Canceled) {
			return nil
		}
		return ctx.Err()
	case <-done:
		return nil
	}
}
