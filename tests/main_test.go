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

// TestMain boots the stack once for the whole package. It is gated by
// RUN_INTEGRATION_TESTS so `go test ./...` stays fast and GPU-free by default.
func TestMain(m *testing.M) {
	// Ballot generation for >16 voters re-executes this test binary as a
	// subprocess; intercept that mode before booting any services.
	integration.RunBallotWorkerIfRequested()

	if v := os.Getenv("RUN_INTEGRATION_TESTS"); v == "" || v == "false" {
		log.Info("skipping davinci-fold integration tests (set RUN_INTEGRATION_TESTS=true)")
		os.Exit(0)
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
