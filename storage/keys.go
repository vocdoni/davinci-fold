package storage

import (
	"encoding/binary"

	"github.com/vocdoni/davinci-fold/types"
)

// electionKey is the storage key for an election: its hex ID.
func electionKey(id types.ElectionID) []byte {
	return []byte(id.String())
}

// subKey builds "<electionHex>/<sub>" so per-election artifacts share a
// scannable prefix and never collide across elections.
func subKey(id types.ElectionID, sub []byte) []byte {
	prefix := append([]byte(id.String()), '/')
	return append(prefix, sub...)
}

// electionScanPrefix is the prefix that matches every per-election artifact of
// an election (used with Iterate to list votes/batches of one election).
func electionScanPrefix(id types.ElectionID) []byte {
	return append([]byte(id.String()), '/')
}

// seqBytes encodes a sequence number big-endian so keys sort in order.
func seqBytes(seq uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, seq)
	return b
}
