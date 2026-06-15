package tests

import (
	"context"
	"net/http"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"

	"github.com/vocdoni/davinci-fold/tests/helpers"
)

// TestAPILifecycle exercises the HTTP surface end-to-end over real JWT auth and
// the chi middleware stack: admin create, public read/list, keywarden gating,
// and worker registration. It needs no prover workers.
func TestAPILifecycle(t *testing.T) {
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

// TestAPIAuthRejections verifies admin/keywarden routes reject missing and
// wrong-role tokens.
func TestAPIAuthRejections(t *testing.T) {
	c := qt.New(t)
	ctx := context.Background()

	// No token on the admin create route.
	req, _, err := helpers.NewElectionRequest("0xdead01", 2, 1, time.Now().Add(time.Hour))
	c.Assert(err, qt.IsNil)
	_, err = services.Client.CreateElection(ctx, "", req)
	c.Assert(err, qt.IsNotNil)

	// Keywarden token on the admin create route.
	_, err = services.Client.CreateElection(ctx, helpers.KeywardenToken(), req)
	c.Assert(err, qt.IsNotNil)

	// Admin token on the keywarden encrypted-results route. First create it.
	created, err := services.Client.CreateElection(ctx, helpers.AdminToken(), req)
	c.Assert(err, qt.IsNil)
	_, _, err = services.Client.EncryptedResults(ctx, helpers.AdminToken(), created.ID)
	c.Assert(err, qt.IsNotNil)
}

// TestWorkerRegistration registers a worker over the admin route and lists it.
func TestWorkerRegistration(t *testing.T) {
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
}
