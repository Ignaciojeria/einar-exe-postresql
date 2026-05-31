package server

import (
	"context"
	"os"
	"strings"
	"time"

	"postgresql/internal/shared/server/middleware"

	"github.com/Ignaciojeria/ioc"
	"github.com/MicahParks/keyfunc/v3"
	"github.com/go-fuego/fuego"
)

var _ = ioc.Register(New)
var _ = ioc.Register(startServer)

type Server struct {
	*fuego.Server
}

func New(jwks keyfunc.Keyfunc) *Server {
	server := fuego.NewServer(fuego.WithAddr(":8000"))
	// Global middleware (aplica a todas las rutas registradas en este server)
	fuego.Use(server, middleware.JWTMiddleware(
		jwks,
		envOrDefault("JWT_ISSUER", "https://einar.exe.xyz:8000"),
		strings.TrimSpace(os.Getenv("JWT_AUDIENCE")),
	))
	return &Server{Server: server}
}

func startServer(
	server *Server,
	shutdowner ioc.Shutdowner,
) error {
	go func() {
		if err := server.Run(); err != nil {
			panic(err)
		}
	}()

	shutdowner.RegisterShutdown(func() error {
		ctx, cancel := context.WithTimeout(
			context.Background(),
			10*time.Second,
		)
		defer cancel()

		return server.Shutdown(ctx)
	})

	return nil
}

func envOrDefault(key, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	return value
}
