import { useQuery } from '@tanstack/react-query'
import { api } from '../api'
import StatusBadge from '../components/StatusBadge'
import CopyHex from '../components/CopyHex'

interface Props {
  onSelect: (id: string) => void
}

function fmtTime(iso: string) {
  if (!iso) return '—'
  const d = new Date(iso)
  if (isNaN(d.getTime())) return '—'
  return d.toLocaleString(undefined, { dateStyle: 'short', timeStyle: 'short' })
}

export default function Elections({ onSelect }: Props) {
  const { data, isLoading } = useQuery({
    queryKey: ['elections'],
    queryFn: api.listElections,
    refetchInterval: 5000,
  })

  const elections = data?.elections ?? []

  return (
    <>
      <div className="section-header">
        <div className="section-title">Elections</div>
        <span style={{ fontSize: 12, color: 'var(--text-muted)' }}>{elections.length} total</span>
      </div>

      {isLoading && <div className="loading"><div className="spinner" /> Loading elections…</div>}

      {!isLoading && elections.length === 0 && (
        <div className="empty">No elections yet.</div>
      )}

      {elections.length > 0 && (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th>Election ID</th>
                <th>Status</th>
                <th>Batch</th>
                <th>Fold every</th>
                <th>End time</th>
                <th>Created</th>
              </tr>
            </thead>
            <tbody>
              {elections.map(e => (
                <tr key={e.id} onClick={() => onSelect(e.id)}>
                  <td><CopyHex value={e.id} truncate={20} /></td>
                  <td><StatusBadge status={e.status} /></td>
                  <td>{e.batchSize}</td>
                  <td>{e.foldEvery}</td>
                  <td style={{ color: 'var(--text-muted)', fontSize: 12 }}>{fmtTime(e.endTime)}</td>
                  <td style={{ color: 'var(--text-muted)', fontSize: 12 }}>{fmtTime(e.createdAt)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </>
  )
}
