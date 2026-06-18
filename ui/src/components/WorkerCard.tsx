import type { WorkerInfo } from '../types'
import StatusBadge from './StatusBadge'

interface Props {
  worker: WorkerInfo
}

export default function WorkerCard({ worker }: Props) {
  const status = worker.banned ? 'banned' : worker.healthy ? 'healthy' : 'unhealthy'

  return (
    <div className="worker-card">
      <div className="worker-card-header">
        <div>
          <div className="worker-name">{worker.name || 'unnamed'}</div>
          <div className="worker-addr">{worker.address}</div>
        </div>
        <StatusBadge status={status} />
      </div>

      <div className="worker-stats">
        <div>
          <div className="worker-stat-label">Queue</div>
          <div className={`worker-stat-val${worker.queueLen > 0 ? ' blue' : ''}`}
               style={{ color: worker.queueLen > 0 ? 'var(--blue)' : 'var(--text)' }}>
            {worker.queueLen}
          </div>
        </div>
        <div>
          <div className="worker-stat-label">Success</div>
          <div className="worker-stat-val" style={{ color: 'var(--green)' }}>{worker.successCount}</div>
        </div>
        <div>
          <div className="worker-stat-label">Failed</div>
          <div className="worker-stat-val" style={{ color: worker.failedCount > 0 ? 'var(--red)' : 'var(--text)' }}>
            {worker.failedCount}
          </div>
        </div>
      </div>
    </div>
  )
}
