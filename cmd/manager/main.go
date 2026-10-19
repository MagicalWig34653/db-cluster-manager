// SPDX-License-Identifier: AGPL-3.0-only

// Command manager runs the manager API and serves the embedded web UI.
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

	"github.com/MagicalWig34653/db-cluster-manager/internal/api"
	"github.com/MagicalWig34653/db-cluster-manager/internal/version"
	"github.com/MagicalWig34653/db-cluster-manager/internal/webui"
)

func main() {
	addr := flag.String("listen", "127.0.0.1:8080", "address to listen on")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version.Get("manager").Version)
		return
	}

	mux := http.NewServeMux()
	mux.Handle("/api/", api.NewHandler())
	mux.Handle("/", webui.Handler())

	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	slog.Info("manager listening", "addr", *addr, "version", version.Version)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("server failed", "err", err)
		os.Exit(1)
	}
}
