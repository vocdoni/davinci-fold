package tests

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/vocdoni/davinci-fold/log"

	"github.com/vocdoni/davinci-fold/tests/helpers"
	"github.com/vocdoni/davinci-zkvm/go-sdk/tests/integration"
)

// services is the in-process davinci-fold stack shared by the integration
// tests. Worker URLs, if any, come from DAVINCI_FOLD_WORKER_URLS.
var (
	services   *helpers.TestServices
	workerURLs []string
)

// TestMain boots the stack once for the whole package when
// RUN_INTEGRATION_TESTS is set (to anything but false). Without it the
// integration tests skip (see requireIntegration), so `go test ./...` stays
// fast and GPU-free; the chaos tests, which build their own stacks against
// fake provers, run either way.
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
	tempDir := os.TempDir() + "/davinci-fold-test-" + time.Now().Format("20060102150405")

	var cleanup func()
	var err error
	services, cleanup, err = helpers.NewTestServices(ctx, tempDir, helpers.Options{
		BatchSize:  2,
		FoldEvery:  1,
		JobTimeout: 30 * time.Minute,
		WorkerURLs: workerURLs,
	})
	if err != nil {
		cancel()
		log.Fatalf("failed to set up test services: %v", err)
	}

	code := m.Run()

	cleanup()
	cancel()
	_ = os.RemoveAll(tempDir)
	os.Exit(code)
}

// requireIntegration skips the calling test unless the integration suite is
// enabled (RUN_INTEGRATION_TESTS).
func requireIntegration(t *testing.T) {
	t.Helper()
	if services == nil {
		t.Skip("integration test: set RUN_INTEGRATION_TESTS=true")
	}
}
