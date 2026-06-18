import { useState } from 'react'

interface Props {
  value: string
  truncate?: number
}

export default function CopyHex({ value, truncate }: Props) {
  const [copied, setCopied] = useState(false)

  function copy() {
    navigator.clipboard.writeText(value).then(() => {
      setCopied(true)
      setTimeout(() => setCopied(false), 1500)
    })
  }

  const display = truncate && value.length > truncate
    ? value.slice(0, truncate) + '…'
    : value

  return (
    <span className="copy-hex">
      <span title={value}>{display}</span>
      <button className="copy-btn" onClick={e => { e.stopPropagation(); copy() }} title="Copy">
        {copied
          ? <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.5"><polyline points="20,6 9,17 4,12"/></svg>
          : <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2"><rect x="9" y="9" width="13" height="13" rx="2"/><path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"/></svg>
        }
      </button>
    </span>
  )
}
