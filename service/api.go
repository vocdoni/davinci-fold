// Package service wires the davinci-fold components together (dependency
// injection) and exposes lifecycle types the entrypoint starts and stops.
package service

import (
	"context"

	"github.com/vocdoni/davinci-fold/api"
	"github.com/vocdoni/davinci-fold/orchestrator"
	"github.com/vocdoni/davinci-fold/workers"
)

// APIService owns the HTTP API lifecycle.
type APIService struct {
	API *api.API
}

// APIConfig configures the API service.
type APIConfig struct {
	Host      string
	Port      int
	JWTSecret []byte
	Engine    *orchestrator.Engine
	Pool      *workers.WorkerManager
	BatchSize int
	FoldEvery int
}

// NewAPI builds and starts the API service.
func NewAPI(ctx context.Context, conf APIConfig) (*APIService, error) {
	a, err := api.New(ctx, &api.Config{
		Host:      conf.Host,
		Port:      conf.Port,
		JWTSecret: conf.JWTSecret,
		Engine:    conf.Engine,
		Pool:      conf.Pool,
		BatchSize: conf.BatchSize,
		FoldEvery: conf.FoldEvery,
	})
	if err != nil {
		return nil, err
	}
	return &APIService{API: a}, nil
}

// Stop shuts the API service down. The HTTP server is bound to the parent
// context passed at construction; nothing to release explicitly yet.
func (s *APIService) Stop() {}
