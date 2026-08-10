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

	"github.com/alikilicw/filecosystem-workers/internal/config"
	"github.com/alikilicw/filecosystem-workers/internal/contracts"
	imageproc "github.com/alikilicw/filecosystem-workers/internal/processor/image"
	"github.com/alikilicw/filecosystem-workers/internal/queue"
	"github.com/alikilicw/filecosystem-workers/internal/storage"
	"github.com/alikilicw/filecosystem-workers/internal/worker"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	if err := run(log); err != nil {
		log.Error("worker terminated", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	processor, err := imageproc.NewProcessor(log, cfg.Concurrency)
	if err != nil {
		return err
	}
	defer processor.Shutdown()

	store := storage.New(cfg.S3)
	if err := store.EnsureBucket(ctx); err != nil {
		return err
	}

	broker := queue.New(cfg.RabbitURL, log)
	imageWorker := worker.NewImageWorker(store, broker, processor, log, cfg.MaxSourceBytes, cfg.JobTimeout)
	// Prefetch matches parallelism so a worker never reserves messages it
	// cannot start on while another instance sits idle.
	broker.RegisterConsumer(contracts.QueueImageJobs, cfg.Concurrency, cfg.Concurrency, imageWorker.Handle)

	if err := broker.Start(ctx); err != nil {
		return err
	}
	defer broker.Close()

	health := &http.Server{
		Addr:              cfg.MetricsAddr,
		Handler:           healthHandler(broker),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		if err := health.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("health server failed", "error", err)
		}
	}()

	log.Info("worker ready", "concurrency", cfg.Concurrency, "queue", contracts.QueueImageJobs, "env", cfg.Env)
	<-ctx.Done()
	log.Info("shutdown signal received")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	return health.Shutdown(shutdownCtx)
}

func healthHandler(broker *queue.Client) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !broker.Healthy() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"ready":false}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ready":true}`))
	})
	return mux
}
