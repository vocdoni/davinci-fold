import type { ElectionStatus } from '../types'

interface Props {
  status: ElectionStatus | 'healthy' | 'unhealthy' | 'banned'
}

export default function StatusBadge({ status }: Props) {
  return <span className={`badge badge-${status}`}>{status}</span>
}
