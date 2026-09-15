// Package api is the davinci-fold HTTP surface: a chi router with the
// davinci-node middleware stack, a typed Error model and JWT-authenticated
// admin/keywarden routes. Vote submission is self-authenticating (ballot proof
// + ECDSA + census), so it carries no JWT.
package api

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	"github.com/vocdoni/davinci-fold/internal"
	"github.com/vocdoni/davinci-fold/internal/assets"
	"github.com/vocdoni/davinci-fold/orchestrator"
	"github.com/vocdoni/davinci-fold/workers"
	"github.com/vocdoni/davinci-fold/log"
)

const maxRequestBodyLog = 512 // Maximum request-body length to log.

// Config is the API server configuration.
type Config struct {
	Host      string
	Port      int
	JWTSecret []byte
	Engine    *orchestrator.Engine
	Pool      *workers.WorkerManager
	BatchSize int
	FoldEvery int
}

// API is the davinci-fold HTTP server.
type API struct {
	router    *chi.Mux
	engine    *orchestrator.Engine
	pool      *workers.WorkerManager
	jwtSecret []byte
	batchSize int
	foldEvery int
	parentCtx context.Context
}

// New builds the API and starts the HTTP server in the background.
func New(ctx context.Context, conf *Config) (*API, error) {
	if conf == nil {
		return nil, fmt.Errorf("missing API configuration")
	}
	if conf.Engine == nil {
		return nil, fmt.Errorf("missing engine instance")
	}
	if len(conf.JWTSecret) == 0 {
		return nil, fmt.Errorf("missing JWT secret")
	}
	a := &API{
		engine:    conf.Engine,
		pool:      conf.Pool,
		jwtSecret: conf.JWTSecret,
		batchSize: conf.BatchSize,
		foldEvery: conf.FoldEvery,
		parentCtx: ctx,
	}
	a.initRouter()

	go func() {
		addr := fmt.Sprintf("%s:%d", conf.Host, conf.Port)
		log.Infow("starting API server", "host", conf.Host, "port", conf.Port)
		if err := http.ListenAndServe(addr, a.router); err != nil {
			log.Fatalf("failed to start the API server: %v", err)
		}
	}()
	return a, nil
}

// Router returns the chi router (for tests).
func (a *API) Router() *chi.Mux {
	return a.router
}

// initRouter wires the middleware stack and all routes.
func (a *API) initRouter() {
	a.router = chi.NewRouter()
	a.router.Use(cors.New(cors.Options{
		AllowedOrigins:   []string{"*"},
		AllowedMethods:   []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type", "X-CSRF-Token"},
		AllowCredentials: false,
		MaxAge:           300,
	}).Handler)
	a.router.Use(loggingMiddleware(maxRequestBodyLog))
	a.router.Use(middleware.Recoverer)
	a.router.Use(middleware.Throttle(100))
	a.router.Use(middleware.ThrottleBacklog(5000, 40000, 60*time.Second))
	a.router.Use(middleware.Timeout(45 * time.Second))

	a.registerHandlers()
}

// registerHandlers registers every HTTP route, logging each at registration.
func (a *API) registerHandlers() {
	register := func(method, endpoint string, h http.HandlerFunc) {
		log.Infow("register handler", "endpoint", endpoint, "method", method)
		a.router.Method(method, endpoint, h)
	}

	register(http.MethodGet, PingEndpoint, func(w http.ResponseWriter, _ *http.Request) { httpWriteOK(w) })
	register(http.MethodGet, InfoEndpoint, a.info)

	// Elections (creation is admin-only; listing/reading is public).
	a.router.With(a.jwtAuth(RoleAdmin)).Post(ElectionsEndpoint, a.createElection)
	log.Infow("register handler", "endpoint", ElectionsEndpoint, "method", "POST (admin)")
	register(http.MethodGet, ElectionsEndpoint, a.listElections)
	register(http.MethodGet, ElectionEndpoint, a.getElection)

	// Votes are self-authenticating.
	register(http.MethodPost, VotesEndpoint, a.newVote)
	register(http.MethodGet, VoteEndpoint, a.getVote)

	// Two-phase finalize handshake (keywarden-authenticated).
	a.router.With(a.jwtAuth(RoleKeywarden)).Get(EncryptedResultsEndpoint, a.encryptedResults)
	log.Infow("register handler", "endpoint", EncryptedResultsEndpoint, "method", "GET (keywarden)")
	a.router.With(a.jwtAuth(RoleKeywarden)).Post(DecryptionKeyEndpoint, a.decryptionKey)
	log.Infow("register handler", "endpoint", DecryptionKeyEndpoint, "method", "POST (keywarden)")
	register(http.MethodGet, ResultsEndpoint, a.results)

	// Worker pool.
	register(http.MethodGet, WorkersEndpoint, a.listWorkers)
	a.router.With(a.jwtAuth(RoleAdmin)).Post(WorkerRegisterEndpoint, a.registerWorker)
	log.Infow("register handler", "endpoint", WorkerRegisterEndpoint, "method", "POST (admin)")

	// Serve the embedded operator UI. Must be last — lowest priority.
	if assets.FS != nil {
		a.router.Handle("/*", http.FileServer(http.FS(assets.FS)))
		log.Infow("serving embedded UI", "path", "/")
	}
}

// info reports orchestrator status. GET /info
func (a *API) info(w http.ResponseWriter, _ *http.Request) {
	var nWorkers, nElections int
	if a.pool != nil {
		nWorkers = len(a.pool.ListWorkerStats())
	}
	if els, err := a.engine.ListElections(); err == nil {
		nElections = len(els)
	}
	httpWriteJSON(w, &InfoResponse{
		Version:   internal.Version,
		BatchSize: a.batchSize,
		FoldEvery: a.foldEvery,
		Workers:   nWorkers,
		Elections: nElections,
	})
}
