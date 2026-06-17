package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/vocdoni/davinci-fold/service"
	"github.com/vocdoni/davinci-fold/storage"
	"github.com/vocdoni/davinci-node/db"
	"github.com/vocdoni/davinci-node/db/metadb"
	"github.com/vocdoni/davinci-fold/log"
)

// Services holds all the running services.
type Services struct {
	Storage      *storage.Storage
	Orchestrator *service.OrchestratorService
	API          *service.APIService
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading configuration: %v\n", err)
		os.Exit(1)
	}

	log.Init(cfg.Log.Level, cfg.Log.Output, nil)
	log.Infow("starting davinci-fold", "version", Version)

	if err := validateConfig(cfg); err != nil {
		log.Fatalf("Invalid configuration: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	services, err := setupServices(ctx, cfg)
	if err != nil {
		log.Fatalf("Failed to setup services: %v", err)
	}
	defer shutdownServices(services)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	sig := <-sigCh
	log.Infow("received signal, shutting down", "signal", sig.String())
}

// setupServices initializes and starts all required services.
func setupServices(ctx context.Context, cfg *Config) (services *Services, err error) {
	services = &Services{}
	defer func() {
		if err != nil {
			shutdownServices(services)
		}
	}()

	log.Infow("initializing storage", "datadir", cfg.Datadir, "type", db.TypePebble)
	storagedb, err := metadb.New(db.TypePebble, cfg.Datadir)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize storage: %w", err)
	}
	services.Storage = storage.New(storagedb)

	log.Infow("starting orchestrator", "batchSize", cfg.Batch.Size, "foldEvery", cfg.Fold.Every)
	services.Orchestrator, err = service.NewOrchestrator(ctx, service.OrchestratorConfig{
		Storage:         services.Storage,
		BatchSize:       cfg.Batch.Size,
		BatchTimeWindow: cfg.Batch.Time,
		FoldEvery:       cfg.Fold.Every,
		WorkerPoll:      cfg.Worker.PollPeriod,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to start orchestrator: %w", err)
	}

	log.Infow("starting API service", "host", cfg.API.Host, "port", cfg.API.Port)
	services.API, err = service.NewAPI(ctx, service.APIConfig{
		Host:      cfg.API.Host,
		Port:      cfg.API.Port,
		JWTSecret: []byte(cfg.API.JWTSecret),
		Engine:    services.Orchestrator.Engine,
		Pool:      services.Orchestrator.Pool,
		BatchSize: cfg.Batch.Size,
		FoldEvery: cfg.Fold.Every,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to start API service: %w", err)
	}

	log.Info("davinci-fold is running, ready to orchestrate folded proving!")
	return services, nil
}

// shutdownServices gracefully shuts down all services in reverse order.
func shutdownServices(services *Services) {
	if services == nil {
		return
	}
	if services.API != nil {
		services.API.Stop()
	}
	if services.Orchestrator != nil {
		services.Orchestrator.Stop()
	}
	if services.Storage != nil {
		services.Storage.Close()
	}
}
