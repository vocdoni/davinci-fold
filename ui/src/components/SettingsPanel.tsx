import { useState } from 'react'

interface Props {
  onClose: () => void
}

export default function SettingsPanel({ onClose }: Props) {
  const [apiUrl, setApiUrl] = useState(localStorage.getItem('davinci-fold:api-url') || '')
  const [adminJwt, setAdminJwt] = useState(localStorage.getItem('davinci-fold:admin-jwt') || '')
  const [saved, setSaved] = useState(false)

  function save() {
    localStorage.setItem('davinci-fold:api-url', apiUrl)
    localStorage.setItem('davinci-fold:admin-jwt', adminJwt)
    setSaved(true)
    setTimeout(() => setSaved(false), 1500)
  }

  return (
    <div className="settings-panel" onClick={e => e.stopPropagation()}>
      <div className="settings-title">
        Settings
        <button className="icon-btn" onClick={onClose}>
          <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.5">
            <line x1="18" y1="6" x2="6" y2="18"/><line x1="6" y1="6" x2="18" y2="18"/>
          </svg>
        </button>
      </div>

      <div className="form-group">
        <label>API Base URL</label>
        <input
          value={apiUrl}
          onChange={e => setApiUrl(e.target.value)}
          placeholder="(same origin)"
        />
        <span style={{ fontSize: 11, color: 'var(--text-muted)' }}>Leave empty to use the current origin.</span>
      </div>

      <div className="form-group">
        <label>Admin JWT</label>
        <textarea
          value={adminJwt}
          onChange={e => setAdminJwt(e.target.value)}
          placeholder="Paste your admin bearer token here…"
          rows={4}
        />
        <span style={{ fontSize: 11, color: 'var(--text-muted)' }}>Required to register workers. Stored only in localStorage.</span>
      </div>

      <button className="btn btn-primary" onClick={save}>
        {saved ? '✓ Saved' : 'Save'}
      </button>
    </div>
  )
}
