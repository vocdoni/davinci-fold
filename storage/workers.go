package storage

import (
	"time"

	"github.com/vocdoni/davinci-fold/types"
)

// SetWorker stores a worker registration, replacing the one at the same
// address.
func (s *Storage) SetWorker(w *types.WorkerRegistration) error {
	if w.RegisteredAt.IsZero() {
		w.RegisteredAt = time.Now()
	}
	return s.setArtifact(workerPrefix, []byte(w.Address), w)
}

// DeleteWorker removes the registration of the worker at address.
func (s *Storage) DeleteWorker(address string) error {
	return s.deleteArtifact(workerPrefix, []byte(address))
}

// ListWorkers returns every stored worker registration.
func (s *Storage) ListWorkers() ([]*types.WorkerRegistration, error) {
	var out []*types.WorkerRegistration
	if err := s.iterateArtifacts(workerPrefix, func(_, v []byte) bool {
		var w types.WorkerRegistration
		if err := DecodeArtifact(v, &w); err == nil {
			out = append(out, &w)
		}
		return true
	}); err != nil {
		return nil, err
	}
	return out, nil
}
