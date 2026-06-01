package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"postgresql/internal/tunnel"
	"strings"
	"syscall"
)

func main() {
	var (
		api      = flag.String("api", envOrDefault("PGTUNNEL_API", "https://postgresql.exe.xyz"), "API base URL")
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
	tunnelURL, err := tunnel.TunnelURL(*api, *project)
	if err != nil {
		log.Fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Printf("listening on %s -> %s", *listen, strings.TrimSpace(tunnelURL))
	if err := tunnel.Serve(ctx, *listen, tunnel.Options{
		APIBaseURL:   *api,
		Project:      *project,
		Token:        *token,
		DevSub:       *devSub,
		InsecureAuth: *insecure,
	}, log.Printf); err != nil {
		log.Fatal(err)
	}
}

func envOrDefault(key, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	return value
}
