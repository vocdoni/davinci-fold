package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/vocdoni/davinci-fold/workers"
)

// listWorkers returns the current prover-worker pool. GET /workers
func (a *API) listWorkers(w http.ResponseWriter, _ *http.Request) {
	if a.pool == nil {
		httpWriteJSON(w, &WorkersResponse{Workers: []*workers.WorkerInfo{}})
		return
	}
	httpWriteJSON(w, &WorkersResponse{Workers: a.pool.ListWorkerStats()})
}

// registerWorker adds a prover worker to the pool. POST /workers/register (admin)
func (a *API) registerWorker(w http.ResponseWriter, r *http.Request) {
	subject := subjectFromContext(r.Context())
	if a.pool == nil {
		ErrGenericInternalServerError.With("no worker pool configured").Write(w)
		return
	}
	var req WorkerRegisterRequest
	if err := httpReadJSON(r, &req); err != nil {
		ErrMalformedBody.WithErr(err).Write(w)
		return
	}
	if req.Address == "" {
		ErrMalformedWorkerInfo.With("missing worker address").Write(w)
		return
	}
	worker := a.pool.AddWorker(req.Address, req.Name)
	a.engine.AuditWorkerRegister(subject, req.Address)
	httpWriteJSON(w, worker.Info(workers.DefaultWorkerBanRules))
}

// removeWorker takes a prover worker out of the pool. Its running jobs count
// as failed and are sent to other workers.
// DELETE /workers/{workerID} (admin)
func (a *API) removeWorker(w http.ResponseWriter, r *http.Request) {
	subject := subjectFromContext(r.Context())
	if a.pool == nil {
		ErrWorkerNotFound.Write(w)
		return
	}
	worker, ok := a.pool.WorkerByID(chi.URLParam(r, WorkerURLParam))
	if !ok {
		ErrWorkerNotFound.Write(w)
		return
	}
	a.pool.RemoveWorker(worker.Address)
	a.engine.AuditWorkerRemove(subject, worker.Address)
	httpWriteOK(w)
}
