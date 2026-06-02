package in

import (
	"postgresql/internal/shared/server"

	"github.com/Ignaciojeria/ioc"
	"github.com/go-fuego/fuego"
)

var _ = ioc.Register(helloHandler)

func helloHandler(s *server.Server) {
	fuego.Get(s.Server, "/", func(c fuego.ContextNoBody) (string, error) {
		return "Hello, World!", nil
	})
}
