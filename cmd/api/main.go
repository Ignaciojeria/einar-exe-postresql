package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"

	_ "postgresql/internal/adapter/in/web"
	_ "postgresql/internal/adapter/out/postgres"
	_ "postgresql/internal/shared/jwks"
	_ "postgresql/internal/shared/server"

	"github.com/Ignaciojeria/ioc"
)

func main() {
	if err := ioc.LoadDependencies(); err != nil {
		log.Fatal(err)
	}
	// Wait for termination signal (e.g. Ctrl+C or Kubernetes SIGTERM)
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)
	<-quit

	// Execute graceful shutdown
	if err := ioc.Shutdown(); err != nil {
		log.Fatalf("Shutdown errors: %v", err)
	}
}
