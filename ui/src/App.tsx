import { useState } from 'react'
import Overview from './views/Overview'
import Workers from './views/Workers'
import Elections from './views/Elections'
import ElectionDetail from './views/ElectionDetail'
import SettingsPanel from './components/SettingsPanel'

type View = 'overview' | 'workers' | 'elections' | { type: 'election'; id: string }

export default function App() {
  const [view, setView] = useState<View>('overview')
  const [settingsOpen, setSettingsOpen] = useState(false)

  const tab = typeof view === 'string' ? view : 'elections'

  return (
    <>
      <nav className="nav">
        <div className="nav-inner">
          <div className="nav-brand">
            <svg width="18" height="18" viewBox="0 0 18 18" fill="none">
              <polygon points="9,1 17,5 17,13 9,17 1,13 1,5" fill="none" stroke="currentColor" strokeWidth="1.5"/>
              <polygon points="9,5 13,7 13,11 9,13 5,11 5,7" fill="currentColor" opacity="0.3"/>
            </svg>
            davinci<span>-fold</span>
          </div>

          <div className="nav-tabs">
            {(['overview', 'workers', 'elections'] as const).map(t => (
              <button
                key={t}
                className={`nav-tab${tab === t ? ' active' : ''}`}
                onClick={() => setView(t)}
              >
                {t.charAt(0).toUpperCase() + t.slice(1)}
              </button>
            ))}
          </div>

          <div className="nav-actions">
            <button
              className="icon-btn"
              title="Settings"
              onClick={() => setSettingsOpen(o => !o)}
            >
              <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2">
                <circle cx="12" cy="12" r="3"/>
                <path d="M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 0 1-2.83 2.83l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 0 1-4 0v-.09A1.65 1.65 0 0 0 9 19.4a1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 0 1-2.83-2.83l.06-.06A1.65 1.65 0 0 0 4.68 15a1.65 1.65 0 0 0-1.51-1H3a2 2 0 0 1 0-4h.09A1.65 1.65 0 0 0 4.6 9a1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 0 1 2.83-2.83l.06.06A1.65 1.65 0 0 0 9 4.68a1.65 1.65 0 0 0 1-1.51V3a2 2 0 0 1 4 0v.09a1.65 1.65 0 0 0 1 1.51 1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 0 1 2.83 2.83l-.06.06A1.65 1.65 0 0 0 19.4 9a1.65 1.65 0 0 0 1.51 1H21a2 2 0 0 1 0 4h-.09a1.65 1.65 0 0 0-1.51 1z"/>
              </svg>
            </button>
          </div>
        </div>
      </nav>

      {settingsOpen && (
        <div className="overlay" onClick={e => e.target === e.currentTarget && setSettingsOpen(false)}>
          <SettingsPanel onClose={() => setSettingsOpen(false)} />
        </div>
      )}

      <main className="page">
        {view === 'overview' && <Overview onNavigate={setView} />}
        {view === 'workers' && <Workers />}
        {view === 'elections' && (
          <Elections onSelect={id => setView({ type: 'election', id })} />
        )}
        {typeof view === 'object' && view.type === 'election' && (
          <ElectionDetail
            id={view.id}
            onBack={() => setView('elections')}
          />
        )}
      </main>
    </>
  )
}
