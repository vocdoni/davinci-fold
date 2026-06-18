import { useQuery } from '@tanstack/react-query'
import { api } from '../api'
import type { ElectionStatus } from '../types'
import StatusBadge from '../components/StatusBadge'

type View = 'overview' | 'workers' | 'elections' | { type: 'election'; id: string }

interface Props {
  onNavigate: (v: View) => void
}

const STATUS_ORDER: ElectionStatus[] = ['active', 'ended', 'decrypting', 'finalizing', 'results', 'created', 'paused', 'canceled']

export default function Overview({ onNavigate }: Props) {
  const infoQ = useQuery({ queryKey: ['info'], queryFn: api.getInfo, refetchInterval: 5000 })
  const workersQ = useQuery({ queryKey: ['workers'], queryFn: api.getWorkers, refetchInterval: 5000 })
  const electionsQ = useQuery({ queryKey: ['elections'], queryFn: api.listElections, refetchInterval: 5000 })

  const info = infoQ.data
  const workers = workersQ.data?.workers ?? []
  const elections = electionsQ.data?.elections ?? []

  const healthyWorkers = workers.filter(w => w.healthy && !w.banned).length
  const statusCounts = elections.reduce<Partial<Record<ElectionStatus, number>>>((acc, e) => {
    acc[e.status] = (acc[e.status] ?? 0) + 1
    return acc
  }, {})

  return (
    <>
      <div className="section-header" style={{ marginBottom: 20 }}>
        <div className="section-title">Overview</div>
        {info && <span style={{ fontSize: 12, color: 'var(--text-muted)', fontFamily: 'var(--font-mono)' }}>v{info.version}</span>}
      </div>

      <div className="stat-grid">
        <div className="stat-card" style={{ cursor: 'pointer' }} onClick={() => onNavigate('workers')}>
          <div className="stat-label">Workers</div>
          <div className={`stat-value ${healthyWorkers > 0 ? 'green' : workers.length > 0 ? 'red' : ''}`}>
            {infoQ.isLoading ? '—' : `${healthyWorkers} / ${workers.length}`}
          </div>
          <div style={{ fontSize: 11, color: 'var(--text-muted)', marginTop: 6 }}>healthy / total</div>
        </div>

        <div className="stat-card" style={{ cursor: 'pointer' }} onClick={() => onNavigate('elections')}>
          <div className="stat-label">Elections</div>
          <div className="stat-value blue">{infoQ.isLoading ? '—' : elections.length}</div>
          <div style={{ fontSize: 11, color: 'var(--text-muted)', marginTop: 6 }}>total</div>
        </div>

        <div className="stat-card">
          <div className="stat-label">Batch Size</div>
          <div className="stat-value">{info?.batchSize ?? '—'}</div>
          <div style={{ fontSize: 11, color: 'var(--text-muted)', marginTop: 6 }}>votes per batch</div>
        </div>

        <div className="stat-card">
          <div className="stat-label">Fold Every</div>
          <div className="stat-value">{info?.foldEvery ?? '—'}</div>
          <div style={{ fontSize: 11, color: 'var(--text-muted)', marginTop: 6 }}>batches per fold</div>
        </div>
      </div>

      {elections.length > 0 && (
        <div className="card">
          <div className="card-title">Elections by status</div>
          <div style={{ display: 'flex', gap: 8, flexWrap: 'wrap' }}>
            {STATUS_ORDER.filter(s => statusCounts[s]).map(s => (
              <div
                key={s}
                style={{ display: 'flex', alignItems: 'center', gap: 8, background: 'var(--surface-2)', borderRadius: 6, padding: '8px 14px', cursor: 'pointer' }}
                onClick={() => onNavigate('elections')}
              >
                <StatusBadge status={s} />
                <span style={{ fontSize: 18, fontWeight: 700 }}>{statusCounts[s]}</span>
              </div>
            ))}
          </div>
        </div>
      )}

      {workers.length > 0 && (
        <div className="card" style={{ marginTop: 16 }}>
          <div className="card-title">Worker pool</div>
          <div style={{ display: 'flex', flexDirection: 'column', gap: 8 }}>
            {workers.map(w => (
              <div key={w.address} style={{ display: 'flex', alignItems: 'center', gap: 12, padding: '10px 0', borderBottom: '1px solid var(--border)' }}>
                <StatusBadge status={w.banned ? 'banned' : w.healthy ? 'healthy' : 'unhealthy'} />
                <span style={{ fontFamily: 'var(--font-mono)', fontSize: 12, color: 'var(--text-muted)', flex: 1 }}>{w.address}</span>
                {w.name && <span style={{ fontSize: 12, color: 'var(--text)' }}>{w.name}</span>}
                {w.queueLen > 0 && <span style={{ fontSize: 12, color: 'var(--blue)' }}>queue: {w.queueLen}</span>}
              </div>
            ))}
          </div>
        </div>
      )}

      {elections.length === 0 && workers.length === 0 && !infoQ.isLoading && (
        <div className="empty">No workers or elections yet. Use the Settings panel to set your admin JWT, then register a worker.</div>
      )}
    </>
  )
}
