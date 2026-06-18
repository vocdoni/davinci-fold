export interface InfoResponse {
  version: string
  batchSize: number
  foldEvery: number
  workers: number
  elections: number
}

export interface WorkerInfo {
  address: string
  name: string
  healthy: boolean
  queueLen: number
  banned: boolean
  successCount: number
  failedCount: number
}

export interface WorkersResponse {
  workers: WorkerInfo[]
}

export type ElectionStatus =
  | 'created'
  | 'active'
  | 'ended'
  | 'decrypting'
  | 'finalizing'
  | 'results'
  | 'paused'
  | 'canceled'

export interface ElectionResponse {
  id: string
  status: ElectionStatus
  batchSize: number
  foldEvery: number
  endTime: string
  createdAt: string
}

export interface ElectionsResponse {
  elections: ElectionResponse[]
}

export interface ResultsResponse {
  electionID: string
  tally: number[]
  programVK: string
  rootCVadcopFinal: string
  publicValues: string
  proofBytes: string
  finalizedAt?: string
}

export interface ApiError {
  code: number
  error: string
}
