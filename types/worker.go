package types

import "time"

// WorkerRegistration is a registered prover, stored so the pool is rebuilt
// when the orchestrator restarts.
type WorkerRegistration struct {
	Address      string    `cbor:"address"` // base URL
	Name         string    `cbor:"name,omitempty"`
	RegisteredAt time.Time `cbor:"registeredAt"`
}
