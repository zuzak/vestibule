package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// version is filled in at build time via -ldflags "-X main.version=..."
var version = "dev"

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	path := os.Getenv("VESTIBULE_CONFIG")
	if path == "" {
		logger.Error("VESTIBULE_CONFIG not set")
		os.Exit(2)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		logger.Error("config load failed", "error", err.Error())
		os.Exit(2)
	}

	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector())
	reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	m := newMetrics(reg)

	p, err := newProxy(cfg, logger, m, version)
	if err != nil {
		logger.Error("proxy init failed", "error", err.Error())
		os.Exit(2)
	}

	apiSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           p.buildMux(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	metricsSrv := &http.Server{
		Addr:              cfg.MetricsListen,
		Handler:           promhttp.HandlerFor(reg, promhttp.HandlerOpts{Registry: reg}),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 2)
	go func() {
		logger.Info("api listening", "addr", cfg.Listen, "version", version)
		if err := apiSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()
	go func() {
		logger.Info("metrics listening", "addr", cfg.MetricsListen)
		if err := metricsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		logger.Info("shutdown signal received")
	case err := <-errCh:
		logger.Error("server error", "error", err.Error())
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = apiSrv.Shutdown(shutdownCtx)
	_ = metricsSrv.Shutdown(shutdownCtx)
}
