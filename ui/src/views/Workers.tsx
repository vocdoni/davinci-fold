import { useState } from 'react'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { api } from '../api'
import WorkerCard from '../components/WorkerCard'

export default function Workers() {
  const qc = useQueryClient()
  const workersQ = useQuery({ queryKey: ['workers'], queryFn: api.getWorkers, refetchInterval: 5000 })

  const [addr, setAddr] = useState('')
  const [name, setName] = useState('')
  const [formErr, setFormErr] = useState('')
  const [showForm, setShowForm] = useState(false)

  const register = useMutation({
    mutationFn: () => api.registerWorker(addr.trim(), name.trim()),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['workers'] })
      setAddr('')
      setName('')
      setFormErr('')
      setShowForm(false)
    },
    onError: (e: Error) => setFormErr(e.message),
  })

  const workers = workersQ.data?.workers ?? []

  return (
    <>
      <div className="section-header">
        <div className="section-title">Workers</div>
        <button className="btn btn-primary btn-sm" onClick={() => setShowForm(o => !o)}>
          + Register worker
        </button>
      </div>

      {showForm && (
        <div className="card" style={{ marginBottom: 20 }}>
          <div className="card-title">Register prover worker</div>
          {formErr && <div className="error-box">{formErr}</div>}
          <div className="form-row" style={{ marginBottom: 12 }}>
            <div className="form-group" style={{ flex: 2 }}>
              <label>Worker URL</label>
              <input
                value={addr}
                onChange={e => setAddr(e.target.value)}
                placeholder="http://10.200.0.26:8080"
              />
            </div>
            <div className="form-group">
              <label>Name (optional)</label>
              <input
                value={name}
                onChange={e => setName(e.target.value)}
                placeholder="gpu-0"
              />
            </div>
            <button
              className="btn btn-primary"
              onClick={() => register.mutate()}
              disabled={!addr || register.isPending}
              style={{ alignSelf: 'flex-end' }}
            >
              {register.isPending ? 'Registering…' : 'Register'}
            </button>
          </div>
          <span style={{ fontSize: 11, color: 'var(--text-muted)' }}>
            Admin JWT must be set in Settings. The worker must be reachable from the orchestrator.
          </span>
        </div>
      )}

      {workersQ.isLoading && (
        <div className="loading"><div className="spinner" /> Loading workers…</div>
      )}

      {!workersQ.isLoading && workers.length === 0 && (
        <div className="empty">No workers registered yet.</div>
      )}

      {workers.length > 0 && (
        <div className="worker-grid">
          {workers.map(w => <WorkerCard key={w.address} worker={w} />)}
        </div>
      )}
    </>
  )
}
