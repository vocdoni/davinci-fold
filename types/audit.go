package types

import "time"

// AuditRecord is an accountability entry written for each mutating,
// authenticated API call.
type AuditRecord struct {
	Subject    string     `cbor:"subject"` // JWT subject (admin/keywarden identity)
	Role       string     `cbor:"role"`    // role that authorized the action
	Action     string     `cbor:"action"`  // e.g. "create_election", "register_worker"
	ElectionID ElectionID `cbor:"electionID,omitempty"`
	Timestamp  time.Time  `cbor:"timestamp"`
}
