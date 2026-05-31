package jwks

import (
	"github.com/Ignaciojeria/ioc"
	"github.com/MicahParks/keyfunc/v3"
)

var _ = ioc.Register(New)

func New() (keyfunc.Keyfunc, error) {
	return keyfunc.NewDefault([]string{"https://einar.exe.xyz:8000/.well-known/jwks"})
}
