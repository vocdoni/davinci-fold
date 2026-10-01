package tests

import (
	"context"
	"os"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/vocdoni/davinci-node/db"
	"github.com/vocdoni/davinci-node/db/metadb"

	"github.com/vocdoni/davinci-fold/log"
	"github.com/vocdoni/davinci-fold/storage"
	"github.com/vocdoni/davinci-fold/tests/helpers"
	"github.com/vocdoni/davinci-zkvm/go-sdk/tests/integration"
)

// services is the davinci-fold the integration tests talk to: the
// in-process stack, or a client of the external orchestrator at
// DAVINCI_FOLD_URL. Worker URLs, if any, come from DAVINCI_FOLD_WORKER_URLS;
// the tests register them through the API.
var (
	services   *helpers.TestServices
	workerURLs []string
	// stack is the in-process orchestrator, nil with an external one.
	stack *inProcess
	// remote is the external orchestrator, nil with the in-process one.
	remote *external
)

// inProcess is the in-process orchestrator and what restarts it.
type inProcess struct {
	ctx  context.Context
	dir  string
	opts helpers.Options
	stop func()
}

// external is an orchestrator running in a Docker container, reached at
// DAVINCI_FOLD_URL. DAVINCI_FOLD_CONTAINER names its container, which the
// tests crash and restart through the Docker Engine API, and
// DAVINCI_FOLD_DATADIR is its data directory as mounted for the tests, read
// once it is stopped.
type external struct {
	container string
	datadir   string
	docker    *helpers.Docker
}

// TestMain boots the stack once for the whole package when
// RUN_INTEGRATION_TESTS is set (to anything but false), or connects to the
// external orchestrator at DAVINCI_FOLD_URL. Without RUN_INTEGRATION_TESTS
// the integration tests skip (see requireIntegration), so `go test ./...`
// stays fast and GPU-free; the chaos tests, which build their own stacks
// against fake provers, run either way.
func TestMain(m *testing.M) {
	// Ballot generation for >16 voters re-executes this test binary as a
	// subprocess; intercept that mode before booting any services.
	integration.RunBallotWorkerIfRequested()

	if v := os.Getenv("RUN_INTEGRATION_TESTS"); v == "" || v == "false" {
		log.Info("skipping davinci-fold integration tests (set RUN_INTEGRATION_TESTS=true)")
		os.Exit(m.Run())
	}

	log.Init(log.LogLevelDebug, "stdout", nil)
	workerURLs = helpers.WorkerURLsFromEnv()
	ctx, cancel := context.WithCancel(context.Background())

	if url := os.Getenv("DAVINCI_FOLD_URL"); url != "" {
		services = &helpers.TestServices{BaseURL: url, Client: helpers.NewClient(url)}
		remote = &external{
			container: os.Getenv("DAVINCI_FOLD_CONTAINER"),
			datadir:   os.Getenv("DAVINCI_FOLD_DATADIR"),
			docker:    helpers.NewDocker(),
		}
		if err := services.Client.WaitReady(ctx, time.Minute); err != nil {
			cancel()
			log.Fatalf("orchestrator at %s: %v", url, err)
		}
		code := m.Run()
		cancel()
		os.Exit(code)
	}

	stack = &inProcess{
		ctx: ctx,
		dir: os.TempDir() + "/davinci-fold-test-" + time.Now().Format("20060102150405"),
		opts: helpers.Options{
			BatchSize:  2,
			FoldEvery:  1,
			JobTimeout: 30 * time.Minute,
		},
	}
	if err := stack.start(); err != nil {
		cancel()
		log.Fatalf("failed to set up test services: %v", err)
	}

	code := m.Run()

	stack.stop()
	cancel()
	_ = os.RemoveAll(stack.dir)
	os.Exit(code)
}

// start boots the in-process stack on its data directory.
func (p *inProcess) start() error {
	svc, stop, err := helpers.NewTestServices(p.ctx, p.dir, p.opts)
	if err != nil {
		return err
	}
	services, p.stop = svc, stop
	return nil
}

// requireIntegration skips the calling test unless the integration suite is
// enabled (RUN_INTEGRATION_TESTS).
func requireIntegration(t *testing.T) {
	t.Helper()
	if services == nil {
		t.Skip("integration test: set RUN_INTEGRATION_TESTS=true")
	}
}

// crashOrchestrator stops the orchestrator where it is and starts it again
// from its storage. An external one is killed with SIGKILL and started
// again; the in-process one is stopped and booted again on its data
// directory.
func crashOrchestrator(t *testing.T) {
	t.Helper()
	ctx := t.Context()
	if remote == nil {
		stack.stop()
		qt.Assert(t, stack.start(), qt.IsNil)
		return
	}
	qt.Assert(t, remote.container != "", qt.IsTrue, qt.Commentf("DAVINCI_FOLD_CONTAINER is not set"))
	qt.Assert(t, remote.docker.Kill(ctx, remote.container), qt.IsNil)
	qt.Assert(t, remote.docker.Start(ctx, remote.container), qt.IsNil)
	qt.Assert(t, services.Client.WaitReady(ctx, time.Minute), qt.IsNil)
}

// orchestratorStore returns the orchestrator's storage, to read its
// records. An external orchestrator is stopped first, so the test calls it
// once it needs nothing more from it.
func orchestratorStore(t *testing.T) *storage.Storage {
	t.Helper()
	if remote == nil {
		return services.Storage
	}
	qt.Assert(t, remote.container != "" && remote.datadir != "", qt.IsTrue,
		qt.Commentf("DAVINCI_FOLD_CONTAINER and DAVINCI_FOLD_DATADIR must be set"))
	qt.Assert(t, remote.docker.Stop(t.Context(), remote.container, 30*time.Second), qt.IsNil)
	kv, err := metadb.New(db.TypePebble, remote.datadir)
	qt.Assert(t, err, qt.IsNil)
	store := storage.New(kv)
	t.Cleanup(store.Close)
	return store
}

// orchestratorLogs returns what the external orchestrator logged, nil for
// the in-process one, whose log is the test output.
func orchestratorLogs(t *testing.T) []byte {
	t.Helper()
	if remote == nil {
		return nil
	}
	qt.Assert(t, remote.container != "", qt.IsTrue, qt.Commentf("DAVINCI_FOLD_CONTAINER is not set"))
	logs, err := remote.docker.Logs(t.Context(), remote.container)
	qt.Assert(t, err, qt.IsNil)
	return logs
}
