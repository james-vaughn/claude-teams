package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
	dbPath := flag.String("db", "team-server.db", "path to the SQLite database file")
	maxSessions := flag.Int("max-sessions", 100, "maximum number of concurrent team sessions")
	rotateToken := flag.Bool("rotate-token", false, "generate a new server token, replacing the stored one")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	store, err := relay.OpenStore(ctx, *dbPath, *maxSessions)
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close()

	token, err := resolveToken(ctx, store, *rotateToken)
	if err != nil {
		log.Fatalf("token: %v", err)
	}

	server := &http.Server{
		Addr:              fmt.Sprintf(":%d", *port),
		Handler:           relay.NewHandler(store, token),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
	}

	go func() {
		log.Printf("team-server listening on %s (db %s)", server.Addr, *dbPath)
		fmt.Printf("\nServer token: %s\nShare it with teammates along with the server URL and a session id.\n\n", token)
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

func resolveToken(ctx context.Context, store *relay.Store, rotate bool) (string, error) {
	if !rotate {
		stored, err := store.Token(ctx)
		if err != nil || stored != "" {
			return stored, err
		}
	}
	generated, err := generateToken()
	if err != nil {
		return "", err
	}
	return generated, store.SetToken(ctx, generated)
}

func generateToken() (string, error) {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
