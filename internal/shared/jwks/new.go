package jwks

import (
	"os"
	"strings"

	"github.com/Ignaciojeria/ioc"
	"github.com/MicahParks/keyfunc/v3"
)

var _ = ioc.Register(New)

func New() (keyfunc.Keyfunc, error) {
	urls := strings.Split(envOrDefault("JWKS_URLS", "https://einar.exe.xyz:8000/.well-known/jwks"), ",")
	for i := range urls {
		urls[i] = strings.TrimSpace(urls[i])
	}
	return keyfunc.NewDefault(urls)
}

func envOrDefault(key, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	return value
}
