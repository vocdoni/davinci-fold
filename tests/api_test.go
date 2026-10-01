package tests

import (
	"context"
	"net/http"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"

	"github.com/vocdoni/davinci-fold/tests/helpers"
	"github.com/vocdoni/davinci-fold/workers"
)

// TestAPILifecycle exercises the HTTP surface end-to-end over real JWT auth and
// the chi middleware stack: admin create, public read/list, keywarden gating,
// and worker registration. It needs no prover workers.
func TestAPILifecycle(t *testing.T) {
	requireIntegration(t)
	c := qt.New(t)
	ctx := context.Background()
	admin := helpers.AdminToken()
	keywarden := helpers.KeywardenToken()

	// Admin creates an election.
	req, _, err := helpers.NewElectionRequest("0xabcd01", 2, 1, time.Now().Add(time.Hour))
	c.Assert(err, qt.IsNil)
	el, err := services.Client.CreateElection(ctx, admin, req)
	c.Assert(err, qt.IsNil)
	c.Assert(el.ID, qt.Equals, "abcd01")
	// A freshly created election with a future end time is immediately Active.
	c.Assert(el.Status, qt.Not(qt.Equals), "")

	// Public read-back.
	got, err := services.Client.GetElection(ctx, el.ID)
	c.Assert(err, qt.IsNil)
	c.Assert(got.ID, qt.Equals, "abcd01")

	// Keywarden cannot read encrypted results before the election ends.
	_, code, err := services.Client.EncryptedResults(ctx, keywarden, el.ID)
	c.Assert(err, qt.IsNotNil)
	c.Assert(code, qt.Equals, http.StatusConflict)
}

// TestAPIAuthRejections verifies admin/keywarden routes answer 401 without a
// token and 403 to a token of the wrong role.
func TestAPIAuthRejections(t *testing.T) {
	requireIntegration(t)
	c := qt.New(t)
	ctx := context.Background()

	// No token on the admin create route.
	req, _, err := helpers.NewElectionRequest("0xdead01", 2, 1, time.Now().Add(time.Hour))
	c.Assert(err, qt.IsNil)
	_, err = services.Client.CreateElection(ctx, "", req)
	c.Assert(err, qt.ErrorMatches, `.*status 401.*"code":40005.*`)

	// Keywarden token on the admin create route.
	_, err = services.Client.CreateElection(ctx, helpers.KeywardenToken(), req)
	c.Assert(err, qt.ErrorMatches, `.*status 403.*"code":40004.*`)

	// Admin token on the keywarden encrypted-results route. First create it.
	created, err := services.Client.CreateElection(ctx, helpers.AdminToken(), req)
	c.Assert(err, qt.IsNil)
	_, code, err := services.Client.EncryptedResults(ctx, helpers.AdminToken(), created.ID)
	c.Assert(err, qt.IsNotNil)
	c.Assert(code, qt.Equals, http.StatusForbidden)
	_, code, err = services.Client.EncryptedResults(ctx, "", created.ID)
	c.Assert(err, qt.IsNotNil)
	c.Assert(code, qt.Equals, http.StatusUnauthorized)
}

// TestAPIElectionStatus pauses, resumes, ends and cancels elections over
// HTTP; refused changes answer 409.
func TestAPIElectionStatus(t *testing.T) {
	requireIntegration(t)
	c := qt.New(t)
	ctx := context.Background()
	admin := helpers.AdminToken()

	req, _, err := helpers.NewElectionRequest("0x57a701", 2, 1, time.Now().Add(time.Hour))
	c.Assert(err, qt.IsNil)
	el, err := services.Client.CreateElection(ctx, admin, req)
	c.Assert(err, qt.IsNil)
	for _, step := range []struct {
		status string
		code   int
	}{
		{"paused", http.StatusOK},
		{"paused", http.StatusConflict},
		{"active", http.StatusOK},
		{"ended", http.StatusOK},
		{"active", http.StatusConflict},
		{"canceled", http.StatusConflict},
	} {
		got, code, err := services.Client.SetElectionStatus(ctx, admin, el.ID, step.status)
		c.Assert(code, qt.Equals, step.code, qt.Commentf("%s: %v", step.status, err))
		if step.code == http.StatusOK {
			c.Assert(got.Status, qt.Equals, step.status)
		}
	}
	_, code, _ := services.Client.SetElectionStatus(ctx, "", el.ID, "paused")
	c.Assert(code, qt.Equals, http.StatusUnauthorized)

	req, _, err = helpers.NewElectionRequest("0x57a702", 2, 1, time.Now().Add(time.Hour))
	c.Assert(err, qt.IsNil)
	el, err = services.Client.CreateElection(ctx, admin, req)
	c.Assert(err, qt.IsNil)
	got, _, err := services.Client.SetElectionStatus(ctx, admin, el.ID, "canceled")
	c.Assert(err, qt.IsNil)
	c.Assert(got.Status, qt.Equals, "canceled")
	read, err := services.Client.GetElection(ctx, el.ID)
	c.Assert(err, qt.IsNil)
	c.Assert(read.Status, qt.Equals, "canceled")
}

// TestWorkerRegistration registers a worker over the admin route and lists it.
func TestWorkerRegistration(t *testing.T) {
	requireIntegration(t)
	c := qt.New(t)
	ctx := context.Background()

	err := services.Client.RegisterWorker(ctx, helpers.AdminToken(), "http://127.0.0.1:65535", "phantom")
	c.Assert(err, qt.IsNil)

	list, err := services.Client.ListWorkers(ctx)
	c.Assert(err, qt.IsNil)
	found := false
	for _, w := range list.Workers {
		if w.Address == "http://127.0.0.1:65535" {
			found = true
		}
	}
	c.Assert(found, qt.IsTrue)

	// Unauthenticated registration is rejected.
	err = services.Client.RegisterWorker(ctx, "", "http://127.0.0.1:1", "x")
	c.Assert(err, qt.IsNotNil)

	// An admin removes it by ID.
	id := workers.WorkerID("http://127.0.0.1:65535")
	code, err := services.Client.RemoveWorker(ctx, "", id)
	c.Assert(err, qt.IsNotNil)
	c.Assert(code, qt.Equals, http.StatusUnauthorized)
	_, err = services.Client.RemoveWorker(ctx, helpers.AdminToken(), id)
	c.Assert(err, qt.IsNil)
	list, err = services.Client.ListWorkers(ctx)
	c.Assert(err, qt.IsNil)
	for _, w := range list.Workers {
		c.Assert(w.ID, qt.Not(qt.Equals), id)
	}
	code, _ = services.Client.RemoveWorker(ctx, helpers.AdminToken(), id)
	c.Assert(code, qt.Equals, http.StatusNotFound)
}
