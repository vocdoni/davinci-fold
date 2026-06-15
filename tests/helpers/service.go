// Package helpers builds the in-process davinci-fold stack (storage, worker
// pool, engine, HTTP API) used by the integration tests. It boots a real HTTP
// server on a discovered free port so tests exercise the same code paths as
// production, including JWT auth and the chi middleware stack.
package helpers

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/golang-jwt/jwt/v4"
	"github.com/vocdoni/davinci-node/db"
	"github.com/vocdoni/davinci-node/db/metadb"

	"github.com/vocdoni/davinci-fold/api"
	"github.com/vocdoni/davinci-fold/orchestrator"
	"github.com/vocdoni/davinci-fold/service"
	"github.com/vocdoni/davinci-fold/storage"
	"github.com/vocdoni/davinci-fold/workers"
)

// TestJWTSecret is the HMAC secret the harness signs admin/keywarden tokens with.
const TestJWTSecret = "davinci-fold-integration-secret"

// TestServices holds the booted in-process stack and a client bound to it.
type TestServices struct {
	BaseURL string
	Storage *storage.Storage
	Engine  *orchestrator.Engine
	Pool    *workers.WorkerManager
	API     *service.APIService
	Client  *Client
}

// Options tunes the orchestrator the harness builds.
type Options struct {
	BatchSize       int
	BatchTimeWindow time.Duration
	FoldEvery       int
	JobTimeout      time.Duration
	WorkerURLs      []string // prover-worker base URLs to register up front
}

// NewTestServices boots storage, the worker pool, the engine, and the HTTP API
// against tempDir, registering any provided worker URLs. The returned cleanup
// stops the services and closes storage.
func NewTestServices(ctx context.Context, tempDir string, opts Options) (*TestServices, func(), error) {
	if opts.BatchSize <= 0 {
		opts.BatchSize = 2
	}
	if opts.FoldEvery <= 0 {
		opts.FoldEvery = 1
	}

	kv, err := metadb.New(db.TypePebble, tempDir)
	if err != nil {
		return nil, nil, fmt.Errorf("create database: %w", err)
	}
	store := storage.New(kv)

	pool := workers.NewWorkerManager(workers.DefaultWorkerBanRules, time.Second)
	pool.Start(ctx)
	for _, url := range opts.WorkerURLs {
		pool.AddWorker(url, "")
	}

	engine, err := orchestrator.NewEngine(store, orchestrator.Options{
		BatchSize:       opts.BatchSize,
		BatchTimeWindow: opts.BatchTimeWindow,
		Pool:            pool,
		FoldEvery:       opts.FoldEvery,
		JobTimeout:      opts.JobTimeout,
	})
	if err != nil {
		pool.Stop()
		store.Close()
		return nil, nil, fmt.Errorf("build engine: %w", err)
	}

	port, err := freePort()
	if err != nil {
		engine.Stop()
		pool.Stop()
		store.Close()
		return nil, nil, fmt.Errorf("find free port: %w", err)
	}

	apiSvc, err := service.NewAPI(ctx, service.APIConfig{
		Host:      "127.0.0.1",
		Port:      port,
		JWTSecret: []byte(TestJWTSecret),
		Engine:    engine,
		Pool:      pool,
		BatchSize: opts.BatchSize,
		FoldEvery: opts.FoldEvery,
	})
	if err != nil {
		engine.Stop()
		pool.Stop()
		store.Close()
		return nil, nil, fmt.Errorf("start API: %w", err)
	}

	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	ts := &TestServices{
		BaseURL: baseURL,
		Storage: store,
		Engine:  engine,
		Pool:    pool,
		API:     apiSvc,
		Client:  NewClient(baseURL),
	}
	if err := ts.Client.WaitReady(ctx, 5*time.Second); err != nil {
		engine.Stop()
		pool.Stop()
		store.Close()
		return nil, nil, fmt.Errorf("API not ready: %w", err)
	}

	cleanup := func() {
		apiSvc.Stop()
		engine.Stop()
		pool.Stop()
		store.Close()
	}
	return ts, cleanup, nil
}

// MintToken signs a JWT for the given role and subject with the harness secret.
func MintToken(role, subject string) (string, error) {
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"role": role,
		"sub":  subject,
		"exp":  time.Now().Add(time.Hour).Unix(),
	})
	return tok.SignedString([]byte(TestJWTSecret))
}

// AdminToken returns a signed admin JWT or fails the harness build.
func AdminToken() string {
	t, _ := MintToken(api.RoleAdmin, "integration-admin")
	return t
}

// KeywardenToken returns a signed keywarden JWT.
func KeywardenToken() string {
	t, _ := MintToken(api.RoleKeywarden, "integration-keywarden")
	return t
}

// freePort asks the OS for an unused TCP port on the loopback interface.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}
