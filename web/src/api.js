// Thin fetch wrapper around the timing-station API.
// In dev, Vite proxies /api to the Go server; in the Docker image nginx does.

const BASE = '/api'

async function request(path, opts = {}) {
  const res = await fetch(BASE + path, {
    headers: { 'Content-Type': 'application/json' },
    ...opts
  })
  const text = await res.text()
  const data = text ? JSON.parse(text) : null
  if (!res.ok) {
    const err = new Error(data?.error?.message || `HTTP ${res.status}`)
    err.status = res.status
    err.code = data?.error?.code || 'unknown'
    throw err
  }
  return data
}

export const api = {
  health: () => request('/health'),
  createBatch: (body) =>
    request('/batches', { method: 'POST', body: JSON.stringify(body) }),
  getBatch: (barcode) => request(`/batches/${encodeURIComponent(barcode)}`),
  listEvents: (barcode) => request(`/batches/${encodeURIComponent(barcode)}/events`),
  postEvent: (barcode, body) =>
    request(`/batches/${encodeURIComponent(barcode)}/events`, {
      method: 'POST',
      body: JSON.stringify(body)
    }),
  undoEvent: (barcode, id, body) =>
    request(`/batches/${encodeURIComponent(barcode)}/events/${id}/undo`, {
      method: 'POST',
      body: JSON.stringify(body)
    })
}
