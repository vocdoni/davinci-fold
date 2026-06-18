import type {
  InfoResponse,
  WorkersResponse,
  WorkerInfo,
  ElectionsResponse,
  ElectionResponse,
  ResultsResponse,
} from './types'

function baseURL(): string {
  return localStorage.getItem('davinci-fold:api-url') || ''
}

function adminToken(): string {
  return localStorage.getItem('davinci-fold:admin-jwt') || ''
}

async function get<T>(path: string, token?: string): Promise<T> {
  const headers: HeadersInit = {}
  if (token) headers['Authorization'] = `Bearer ${token}`
  const res = await fetch(baseURL() + path, { headers })
  if (!res.ok) {
    const body = await res.json().catch(() => ({ error: res.statusText }))
    throw new Error(body.error || `${res.status} ${res.statusText}`)
  }
  return res.json() as Promise<T>
}

async function post<T>(path: string, body: unknown, token?: string): Promise<T> {
  const headers: HeadersInit = { 'Content-Type': 'application/json' }
  if (token) headers['Authorization'] = `Bearer ${token}`
  const res = await fetch(baseURL() + path, {
    method: 'POST',
    headers,
    body: JSON.stringify(body),
  })
  if (!res.ok) {
    const err = await res.json().catch(() => ({ error: res.statusText }))
    throw new Error(err.error || `${res.status} ${res.statusText}`)
  }
  return res.json() as Promise<T>
}

export const api = {
  getInfo: () => get<InfoResponse>('/info'),

  getWorkers: () => get<WorkersResponse>('/workers'),
  registerWorker: (address: string, name: string) =>
    post<WorkerInfo>('/workers/register', { address, name }, adminToken()),

  listElections: () => get<ElectionsResponse>('/elections'),
  getElection: (id: string) => get<ElectionResponse>(`/elections/${id}`),
  getResults: (id: string) => get<ResultsResponse>(`/elections/${id}/results`),
}
