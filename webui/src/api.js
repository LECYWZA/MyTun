// API client for the MyTun console. All calls are same-origin; auth is the
// HttpOnly cookie set by /api/login, so no token juggling in the frontend.

const jsonHeaders = { 'Content-Type': 'application/json' }

export class ApiError extends Error {
  constructor(message, status, body) {
    super(message)
    this.status = status
    this.body = body
  }
}

async function request(path, { method = 'GET', body, raw } = {}) {
  const opts = { method, credentials: 'same-origin', headers: {} }
  if (body !== undefined && !raw) {
    opts.headers = jsonHeaders
    opts.body = JSON.stringify(body)
  } else if (raw) {
    opts.body = raw
  }
  const res = await fetch(path, opts)
  const text = await res.text()
  let data = null
  try {
    data = text ? JSON.parse(text) : null
  } catch {
    data = text
  }
  if (!res.ok) {
    const msg = (data && (data.error || data.message)) || `HTTP ${res.status}`
    throw new ApiError(msg, res.status, data)
  }
  return data
}

export const api = {
  login: (username, password) => request('/api/login', { method: 'POST', body: { username, password } }),
  logout: () => request('/api/logout', { method: 'POST' }),
  health: () => request('/api/health'),
  config: () => request('/api/config'),
  saveConfig: (cfg) => request('/api/config', { method: 'POST', body: cfg }),
  status: () => request('/api/status'),
  apply: () => request('/api/apply', { method: 'POST' }),
  stopAll: () => request('/api/service/stop', { method: 'POST' }),
  changePassword: (payload) => request('/api/change-password', { method: 'POST', body: payload }),
  systemProxy: (action, payload = {}) =>
    request(`/api/system-proxy/${action}`, { method: action === 'status' ? 'GET' : 'POST', body: action === 'status' ? undefined : payload }),
  trafficLogs: (params = {}) => {
    const q = new URLSearchParams(Object.entries(params).filter(([, v]) => v !== '' && v !== undefined && v !== null)).toString()
    return request(`/api/traffic-logs${q ? '?' + q : ''}`)
  },
  clearLogs: () => request('/api/traffic-logs', { method: 'DELETE' }),
  pruneLogs: (params = {}) => {
    const q = new URLSearchParams(Object.entries(params).filter(([, v]) => v !== undefined && v !== null)).toString()
    return request(`/api/traffic-logs/prune${q ? '?' + q : ''}`, { method: 'POST' })
  },
  settings: (payload) => request('/api/settings', { method: 'POST', body: payload }),
  testLatency: (payload) => request('/api/test-latency', { method: 'POST', body: payload }),
  testSpeed: (payload) => request('/api/test-speed', { method: 'POST', body: payload }),
  checkPort: (port) => request(`/api/check-port/${port}`),
  chatHistory: () => request('/api/chat/history'),
  chatSend: (payload) => request('/api/chat/send', { method: 'POST', body: payload }),
  files: () => request('/api/files'),
  deleteFile: (name) => request(`/api/files/${encodeURIComponent(name)}`, { method: 'DELETE' }),
  sendFile: (file, extra = {}) => {
    const fd = new FormData()
    fd.append('file', file)
    for (const [k, v] of Object.entries(extra)) fd.append(k, v ?? '')
    return request('/api/file/send', { method: 'POST', body: fd, raw: true })
  },
  interfaces: () => request('/api/network-interfaces'),
  qos: () => request('/api/qos'),
  setQos: (payload) => request('/api/qos', { method: 'POST', body: payload }),
}

// Live event stream. Reconnects with backoff and fans events out to handlers.
export function createEventStream(onEvent) {
  let ws = null
  let retry = 1000
  let closed = false
  const connect = () => {
    if (closed) return
    const proto = location.protocol === 'https:' ? 'wss' : 'ws'
    ws = new WebSocket(`${proto}://${location.host}/ws`)
    ws.onopen = () => { retry = 1000 }
    ws.onmessage = (ev) => {
      try {
        onEvent(JSON.parse(ev.data))
      } catch { /* ignore malformed frames */ }
    }
    ws.onclose = () => {
      if (closed) return
      setTimeout(connect, retry)
      retry = Math.min(retry * 2, 15000)
    }
    ws.onerror = () => { try { ws.close() } catch { /* noop */ } }
  }
  connect()
  return {
    close() { closed = true; try { ws?.close() } catch { /* noop */ } },
  }
}
