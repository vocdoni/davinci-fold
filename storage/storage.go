// Package storage is the Pebble-backed persistence layer for davinci-fold. It
// owns the election records and lifecycle state, the ordered vote log and
// per-vote status, the exact per-batch prove inputs, the fold-chain
// checkpoints, the State snapshots, the decrypted results and the audit trail,
// plus the reservation/recovery bookkeeping that lets the orchestrator survive
// restarts and worker death.
//
// It mirrors davinci-node's storage conventions: prefixed namespaces over a
// single db.Database, CBOR artifact encoding, a reservation sub-namespace that
// is cleared on startup, a stale-reservation monitor, and in-memory lock maps
// for per-(election,address) and per-voteID dedup.
package storage

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/vocdoni/davinci-node/db"
	"github.com/vocdoni/davinci-node/log"
)

var (
	// ErrNotFound is returned when a key does not exist.
	ErrNotFound = errors.New("not found")
	// ErrKeyAlreadyExists is returned when reserving an already-reserved key.
	ErrKeyAlreadyExists = errors.New("key already exists")
	// ErrNoMoreElements is returned when an iteration finds nothing.
	ErrNoMoreElements = errors.New("no more elements")
)

// Namespace prefixes. Keep them short; they are the first bytes of every key.
var (
	reservationPrefixRoot = []byte("r/")

	electionPrefix   = []byte("el/")   // electionID → Election
	votePrefix       = []byte("v/")    // electionID + voteID → Vote
	voteStatusPrefix = []byte("vs/")   // electionID + voteID → status byte
	voteSeqPrefix    = []byte("vseq/") // electionID → last vote seq (uint64)
	batchPrefix      = []byte("ba/")   // electionID + seq → BatchInput
	foldPrefix       = []byte("fc/")   // electionID → FoldCheckpoint
	snapshotPrefix   = []byte("ss/")   // electionID → State snapshot blob
	resultsPrefix    = []byte("rs/")   // electionID → Results
	auditPrefix      = []byte("au/")   // ts+seq → AuditRecord
)

// reservationBasePrefixes are the artifact namespaces that carry reservations,
// cleared on startup so nothing stays blocked after a crash.
func reservationBasePrefixes() [][]byte {
	return [][]byte{batchPrefix}
}

// reservationRecord stores metadata about a reservation.
type reservationRecord struct {
	Timestamp int64 `cbor:"timestamp"`
}

// Storage manages persisted artifacts with reservations and in-memory locks.
type Storage struct {
	db     db.Database
	ctx    context.Context
	cancel context.CancelFunc

	globalLock sync.Mutex

	// processingAddresses tracks "electionID:address" currently in flight,
	// preventing two concurrent votes from the same voter racing.
	processingAddresses sync.Map
	// processingVoteIDs tracks voteIDs currently in flight.
	processingVoteIDs sync.Map
}

// New creates a Storage over the given database, clears stale reservations
// left by a previous run, and starts the stale-reservation monitor.
func New(database db.Database) *Storage {
	ctx, cancel := context.WithCancel(context.Background())
	s := &Storage{db: database, ctx: ctx, cancel: cancel}
	if err := s.recover(); err != nil {
		log.Errorw(err, "failed to clear stale reservations on startup")
	}
	s.monitorStaleReservations()
	return s
}

// Close stops background work and closes the database.
func (s *Storage) Close() {
	s.cancel()
	defer func() {
		if r := recover(); r != nil {
			if strings.Contains(fmt.Sprintf("%v", r), "closed") {
				log.Warn("storage database already closed")
				return
			}
			log.Errorf("storage close panic: %v", r)
		}
	}()
	if s.db != nil {
		if err := s.db.Close(); err != nil {
			log.Warnw("failed to close storage", "error", err)
		}
	}
}

// recover clears all reservations so blocked artifacts are processable again.
func (s *Storage) recover() error {
	s.globalLock.Lock()
	defer s.globalLock.Unlock()
	if s.db == nil {
		return fmt.Errorf("database not initialized")
	}
	for _, prefix := range reservationBasePrefixes() {
		if err := s.cleanAllReservations(prefix); err != nil {
			if strings.Contains(err.Error(), "pebble: closed") {
				return fmt.Errorf("database closed")
			}
			return fmt.Errorf("clear reservations for %s: %w", prefix, err)
		}
	}
	return nil
}

// monitorStaleReservations periodically frees reservations older than 5m.
func (s *Storage) monitorStaleReservations() {
	ticker := time.NewTicker(60 * time.Second)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-s.ctx.Done():
				return
			case <-ticker.C:
				if err := s.releaseStaleReservations(5 * time.Minute); err != nil {
					log.Warnw("failed to release stale reservations", "error", err)
				}
			}
		}
	}()
}
