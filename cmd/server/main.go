// Command server runs the MoonWalk order countdown service.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"

	"moonwalk/internal/api"
	"moonwalk/internal/order"
	"moonwalk/internal/store"
)

func main() {
	if err := run(); err != nil {
		slog.Error("server stopped", "err", err)
		os.Exit(1)
	}
}

func run() error {
	port := flag.Int("port", 8080, "HTTP port")
	seed := flag.String("seed", "restaurants.json", "restaurants to load at startup (empty = none)")
	retention := flag.Duration("log-retention", 24*time.Hour, "how long estimate log rows are kept")
	tick := flag.Duration("tick", 30*time.Second, "how often active orders are re-estimated")
	flag.Parse()

	// Ctrl+C or SIGTERM starts a clean shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx)
	if err != nil {
		return err
	}
	defer st.Close() // closed last

	svc := order.New(st)
	if *seed != "" {
		restaurants, err := store.LoadSeed(*seed)
		if err != nil {
			return err
		}
		for _, r := range restaurants {
			if err := svc.AddRestaurant(ctx, r); err != nil {
				return fmt.Errorf("seed restaurant %q: %w", r.ID, err)
			}
		}
		slog.Info("restaurants loaded", "count", len(restaurants), "file", *seed)
	}

	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		svc.Run(ctx, *tick, *retention)
	}()
	defer func() { stop(); <-workerDone }() // worker must finish before the DB closes

	gin.SetMode(gin.ReleaseMode)
	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", *port),
		Handler:           api.New(svc),
		ReadHeaderTimeout: 5 * time.Second, // don't let slow clients hang on
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.ListenAndServe() }()
	slog.Info("listening", "addr", "http://localhost"+srv.Addr)

	select {
	case err := <-serveErr:
		return err // e.g. port taken
	case <-ctx.Done():
	}

	slog.Info("shutting down, finishing in-flight requests")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	slog.Info("bye")
	return nil
}
