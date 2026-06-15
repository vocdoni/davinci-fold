package service

import (
	"context"
	"time"

	"github.com/vocdoni/davinci-fold/orchestrator"
	"github.com/vocdoni/davinci-fold/storage"
	"github.com/vocdoni/davinci-fold/workers"
)

// OrchestratorService owns the worker pool and the engine that drives the
// election lifecycle and scatter/gather proving.
type OrchestratorService struct {
	Engine *orchestrator.Engine
	Pool   *workers.WorkerManager
}

// OrchestratorConfig configures the orchestrator service.
type OrchestratorConfig struct {
	Storage         *storage.Storage
	BatchSize       int
	BatchTimeWindow time.Duration
	FoldEvery       int
	JobTimeout      time.Duration
	WorkerPoll      time.Duration
}

// NewOrchestrator builds the worker pool, starts its health loop, and builds the
// engine wired to that pool (enabling scatter/gather proving).
func NewOrchestrator(ctx context.Context, conf OrchestratorConfig) (*OrchestratorService, error) {
	pool := workers.NewWorkerManager(workers.DefaultWorkerBanRules, conf.WorkerPoll)
	pool.Start(ctx)

	engine, err := orchestrator.NewEngine(conf.Storage, orchestrator.Options{
		BatchSize:       conf.BatchSize,
		BatchTimeWindow: conf.BatchTimeWindow,
		Pool:            pool,
		FoldEvery:       conf.FoldEvery,
		JobTimeout:      conf.JobTimeout,
	})
	if err != nil {
		pool.Stop()
		return nil, err
	}
	return &OrchestratorService{Engine: engine, Pool: pool}, nil
}

// Stop halts the engine lifecycle monitor and the worker health loop.
func (s *OrchestratorService) Stop() {
	if s.Engine != nil {
		s.Engine.Stop()
	}
	if s.Pool != nil {
		s.Pool.Stop()
	}
}
