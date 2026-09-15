import { useQuery } from '@tanstack/react-query'
import { api } from '../api'
import type { ElectionStatus } from '../types'
import StatusBadge from '../components/StatusBadge'
import CopyHex from '../components/CopyHex'
import TallyChart from '../components/TallyChart'

interface Props {
  id: string
  onBack: () => void
}

const LIFECYCLE: ElectionStatus[] = ['created', 'active', 'ended', 'decrypting', 'finalizing', 'results']

function stepIndex(status: ElectionStatus): number {
  const i = LIFECYCLE.indexOf(status)
  return i >= 0 ? i : -1
}

function fmtTime(iso: string | undefined) {
  if (!iso) return '—'
  const d = new Date(iso)
  if (isNaN(d.getTime())) return '—'
  return d.toLocaleString(undefined, { dateStyle: 'medium', timeStyle: 'short' })
}

export default function ElectionDetail({ id, onBack }: Props) {
  const elQ = useQuery({
    queryKey: ['election', id],
    queryFn: () => api.getElection(id),
    refetchInterval: 5000,
  })

  const resultsQ = useQuery({
    queryKey: ['election-results', id],
    queryFn: () => api.getResults(id),
    enabled: elQ.data?.status === 'results',
    retry: false,
  })

  const el = elQ.data
  const results = resultsQ.data

  if (elQ.isLoading) {
    return (
      <div>
        <button className="back-btn" onClick={onBack}>← Elections</button>
        <div className="loading"><div className="spinner" /> Loading…</div>
      </div>
    )
  }

  if (elQ.isError || !el) {
    return (
      <div>
        <button className="back-btn" onClick={onBack}>← Elections</button>
        <div className="error-box">Failed to load election: {String(elQ.error)}</div>
      </div>
    )
  }

  const currentStep = stepIndex(el.status)

  return (
    <>
      <button className="back-btn" onClick={onBack}>← Elections</button>

      <div className="election-header">
        <div>
          <div style={{ display: 'flex', alignItems: 'center', gap: 12, marginBottom: 6 }}>
            <h2 style={{ fontSize: 18, fontWeight: 700 }}>Election</h2>
            <StatusBadge status={el.status} />
          </div>
          <div className="election-id">
            <CopyHex value={el.id} />
          </div>
        </div>
      </div>

      {/* Lifecycle timeline */}
      <div className="timeline" style={{ marginBottom: 24 }}>
        {LIFECYCLE.map((step, i) => (
          <div key={step} className="timeline-step">
            {i > 0 && <div className={`timeline-line${i <= currentStep ? ' done' : ''}`} />}
            <div className={`timeline-dot${i < currentStep ? ' done' : i === currentStep ? ' current' : ''}`} />
            <div className={`timeline-label${i < currentStep ? ' done' : i === currentStep ? ' current' : ''}`}>{step}</div>
          </div>
        ))}
      </div>

      {/* Config */}
      <div className="detail-grid">
        <div className="detail-field">
          <div className="detail-field-label">Batch size</div>
          <div className="detail-field-value">{el.batchSize}</div>
        </div>
        <div className="detail-field">
          <div className="detail-field-label">Fold every</div>
          <div className="detail-field-value">{el.foldEvery}</div>
        </div>
        <div className="detail-field">
          <div className="detail-field-label">End time</div>
          <div className="detail-field-value" style={{ fontSize: 13 }}>{fmtTime(el.endTime)}</div>
        </div>
        <div className="detail-field">
          <div className="detail-field-label">Created</div>
          <div className="detail-field-value" style={{ fontSize: 13 }}>{fmtTime(el.createdAt)}</div>
        </div>
        {results?.finalizedAt && (
          <div className="detail-field">
            <div className="detail-field-label">Finalized</div>
            <div className="detail-field-value" style={{ fontSize: 13 }}>{fmtTime(results.finalizedAt)}</div>
          </div>
        )}
      </div>

      {/* Results */}
      {el.status === 'results' && results && (
        <div className="card">
          <div className="card-title">Tally</div>
          <TallyChart tally={results.tally} />

          <div className="proof-section">
            <details>
              <summary>
                <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2"><polyline points="9,18 15,12 9,6"/></svg>
                PLONK proof
              </summary>
              {[
                { label: 'Program VK', value: results.programVK },
                { label: 'Root C Vadcop Final', value: results.rootCVadcopFinal },
                { label: 'Public Values', value: results.publicValues },
                { label: 'Proof Bytes', value: results.proofBytes },
              ].map(f => (
                <div key={f.label} className="proof-field">
                  <div className="proof-field-label">{f.label}</div>
                  <div className="proof-field-value">
                    {f.value}
                    <CopyHex value={f.value} />
                  </div>
                </div>
              ))}
            </details>
          </div>
        </div>
      )}

      {el.status === 'results' && resultsQ.isLoading && (
        <div className="loading"><div className="spinner" /> Loading results…</div>
      )}

      {el.status === 'results' && resultsQ.isError && (
        <div className="error-box">Could not load results: {String(resultsQ.error)}</div>
      )}

      {!['results', 'finalizing', 'decrypting'].includes(el.status) && el.status !== 'canceled' && (
        <div className="empty" style={{ padding: '24px 0', textAlign: 'left' }}>
          {el.status === 'active' && 'Election is active — votes are being accepted.'}
          {el.status === 'ended' && 'Election has ended — batch proving in progress.'}
          {el.status === 'created' && 'Election created, not yet active.'}
          {el.status === 'paused' && 'Election is paused.'}
        </div>
      )}
    </>
  )
}
