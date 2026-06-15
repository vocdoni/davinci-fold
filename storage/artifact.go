package storage

import (
	"fmt"

	"github.com/vocdoni/davinci-node/db/prefixeddb"
)

// setArtifact CBOR-encodes artifact and stores it at prefix+key.
func (s *Storage) setArtifact(prefix, key []byte, artifact any) error {
	data, err := EncodeArtifact(artifact)
	if err != nil {
		return err
	}
	wTx := prefixeddb.NewPrefixedDatabase(s.db, prefix).WriteTx()
	defer wTx.Discard()
	if err := wTx.Set(key, data); err != nil {
		return err
	}
	return wTx.Commit()
}

// getArtifact loads and decodes the artifact at prefix+key into out. Returns
// ErrNotFound if the key is absent.
func (s *Storage) getArtifact(prefix, key []byte, out any) error {
	data, err := prefixeddb.NewPrefixedDatabase(s.db, prefix).Get(key)
	if err != nil {
		return ErrNotFound
	}
	if err := DecodeArtifact(data, out); err != nil {
		return fmt.Errorf("decode artifact: %w", err)
	}
	return nil
}

// deleteArtifact removes the artifact at prefix+key.
func (s *Storage) deleteArtifact(prefix, key []byte) error {
	wTx := prefixeddb.NewPrefixedDatabase(s.db, prefix).WriteTx()
	defer wTx.Discard()
	if err := wTx.Delete(key); err != nil {
		return err
	}
	return wTx.Commit()
}

// listKeys returns every key under prefix (copied).
func (s *Storage) listKeys(prefix []byte) ([][]byte, error) {
	var keys [][]byte
	if err := prefixeddb.NewPrefixedReader(s.db, prefix).Iterate(nil, func(k, _ []byte) bool {
		keys = append(keys, append([]byte(nil), k...))
		return true
	}); err != nil {
		return nil, err
	}
	return keys, nil
}

// iterateArtifacts calls fn for every key/value under prefix. Returning false
// from fn stops iteration. The value slice must not be retained.
func (s *Storage) iterateArtifacts(prefix []byte, fn func(key, value []byte) bool) error {
	return prefixeddb.NewPrefixedReader(s.db, prefix).Iterate(nil, fn)
}
