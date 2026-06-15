package storage

import (
	"fmt"
	"sync/atomic"
	"time"

	"github.com/vocdoni/davinci-fold/types"
)

// auditSeq makes audit keys unique within the same nanosecond.
var auditSeq uint64

// AppendAudit writes an accountability record. Keys are time-ordered.
func (s *Storage) AppendAudit(r *types.AuditRecord) error {
	if r.Timestamp.IsZero() {
		r.Timestamp = time.Now()
	}
	seq := atomic.AddUint64(&auditSeq, 1)
	key := []byte(fmt.Sprintf("%020d-%010d", r.Timestamp.UnixNano(), seq))
	return s.setArtifact(auditPrefix, key, r)
}

// ListAudit returns all audit records in time order.
func (s *Storage) ListAudit() ([]*types.AuditRecord, error) {
	var out []*types.AuditRecord
	if err := s.iterateArtifacts(auditPrefix, func(_, v []byte) bool {
		var r types.AuditRecord
		if err := DecodeArtifact(v, &r); err == nil {
			out = append(out, &r)
		}
		return true
	}); err != nil {
		return nil, err
	}
	return out, nil
}
