package storage

import (
	"fmt"
	"time"

	"github.com/vocdoni/davinci-node/db"
	"github.com/vocdoni/davinci-node/db/prefixeddb"
	"github.com/vocdoni/davinci-node/log"
)

// reservationDB returns the write-capable reservation namespace for a prefix.
func (s *Storage) reservationDB(prefix []byte) db.Database {
	return prefixeddb.NewPrefixedDatabase(
		prefixeddb.NewPrefixedDatabase(s.db, reservationPrefixRoot),
		prefix,
	)
}

// reservationReader returns the read-only reservation namespace for a prefix.
func (s *Storage) reservationReader(prefix []byte) db.Reader {
	return prefixeddb.NewPrefixedReader(
		prefixeddb.NewPrefixedReader(s.db, reservationPrefixRoot),
		prefix,
	)
}

// setReservation reserves key under prefix, failing if already reserved.
func (s *Storage) setReservation(prefix, key []byte) error {
	val, err := EncodeArtifact(&reservationRecord{Timestamp: time.Now().Unix()})
	if err != nil {
		return err
	}
	wTx := s.reservationDB(prefix).WriteTx()
	defer wTx.Discard()
	if _, err := wTx.Get(key); err == nil {
		return ErrKeyAlreadyExists
	}
	if err := wTx.Set(key, val); err != nil {
		return err
	}
	return wTx.Commit()
}

// isReserved reports whether key is reserved under prefix.
func (s *Storage) isReserved(prefix, key []byte) bool {
	_, err := s.reservationReader(prefix).Get(key)
	return err == nil
}

// deleteReservation frees a reservation.
func (s *Storage) deleteReservation(prefix, key []byte) error {
	wTx := s.reservationDB(prefix).WriteTx()
	defer wTx.Discard()
	if err := wTx.Delete(key); err != nil {
		return err
	}
	return wTx.Commit()
}

// cleanAllReservations deletes every reservation under prefix.
func (s *Storage) cleanAllReservations(prefix []byte) error {
	wTx := s.reservationDB(prefix).WriteTx()
	defer wTx.Discard()
	var keys [][]byte
	if err := wTx.Iterate(nil, func(k, _ []byte) bool {
		keys = append(keys, append([]byte(nil), k...))
		return true
	}); err != nil {
		return err
	}
	for _, k := range keys {
		if err := wTx.Delete(k); err != nil {
			return err
		}
	}
	return wTx.Commit()
}

// releaseStaleReservations frees reservations older than maxAge.
func (s *Storage) releaseStaleReservations(maxAge time.Duration) error {
	s.globalLock.Lock()
	defer s.globalLock.Unlock()
	now := time.Now().Unix()
	for _, prefix := range reservationBasePrefixes() {
		if err := s.releaseStaleInPrefix(prefix, now, maxAge); err != nil {
			return err
		}
	}
	return nil
}

func (s *Storage) releaseStaleInPrefix(prefix []byte, now int64, maxAge time.Duration) error {
	wTx := s.reservationDB(prefix).WriteTx()
	defer wTx.Discard()
	var staleKeys [][]byte
	if err := wTx.Iterate(nil, func(k, v []byte) bool {
		r := &reservationRecord{}
		if err := DecodeArtifact(v, r); err != nil {
			staleKeys = append(staleKeys, append([]byte(nil), k...))
			return true
		}
		if now-r.Timestamp > int64(maxAge.Seconds()) {
			staleKeys = append(staleKeys, append([]byte(nil), k...))
		}
		return true
	}); err != nil {
		return fmt.Errorf("iterate stale reservations: %w", err)
	}
	if len(staleKeys) == 0 {
		return nil
	}
	for _, sk := range staleKeys {
		if err := wTx.Delete(sk); err != nil {
			return fmt.Errorf("delete stale reservation: %w", err)
		}
	}
	if err := wTx.Commit(); err != nil {
		return fmt.Errorf("commit stale deletion: %w", err)
	}
	log.Debugw("released stale reservations", "prefix", string(prefix), "count", len(staleKeys))
	return nil
}
