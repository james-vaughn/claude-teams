package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/hudl/claude-teams/internal/relay"
)

func main() {
	port := flag.Int("port", 8080, "port to listen on")
	token := flag.String("token", os.Getenv("TEAM_TOKEN"), "optional shared token clients must send (defaults to $TEAM_TOKEN)")
	flag.Parse()

	server := &http.Server{
		Addr:              fmt.Sprintf(":%d", *port),
		Handler:           relay.NewHandler(relay.NewStore(), *token),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		authMode := "open"
		if *token != "" {
			authMode = "token required"
		}
		log.Printf("team-server listening on %s (%s)", server.Addr, authMode)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("listen: %v", err)
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown: %v", err)
	}
}
