// Package httpx provides shared HTTP server conventions for the
// compliance-suite services: graceful shutdown and full timeout defaults
// (assurance F-5 — SIGTERM must not hard-kill in-flight requests).
package httpx

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// ShutdownTimeout bounds graceful shutdown on SIGTERM/SIGINT.
const ShutdownTimeout = 15 * time.Second

// NewServer builds the standard service server with full timeout defaults
// (ReadHeaderTimeout alone leaves body-read slowloris exposure).
func NewServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
}

// Serve runs srv until it fails or a SIGTERM/SIGINT arrives, then drains
// in-flight requests via http.Server.Shutdown bounded by ShutdownTimeout.
// A nil error means a clean shutdown.
func Serve(srv *http.Server) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errCh := make(chan error, 1)
	go func() {
		log.Printf("listening on %s", srv.Addr)
		errCh <- srv.ListenAndServe()
	}()
	select {
	case err := <-errCh:
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	case <-ctx.Done():
		log.Printf("shutdown signal received; draining in-flight requests (timeout %s)", ShutdownTimeout)
		dctx, cancel := context.WithTimeout(context.Background(), ShutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(dctx); err != nil {
			return fmt.Errorf("graceful shutdown: %w", err)
		}
		return nil
	}
}

// ListenAndServe serves h at addr with graceful shutdown and full timeouts.
func ListenAndServe(addr string, h http.Handler) error {
	return Serve(NewServer(addr, h))
}
