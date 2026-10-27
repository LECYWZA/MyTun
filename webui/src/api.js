// API client for the MyTun console. All calls are same-origin; auth is the
// HttpOnly cookie set by /api/login, so no token juggling in the frontend.

const jsonHeaders = { 'Content-Type': 'application/json' }

// Access token lives in memory/sessionStorage (1h); the 7-day refresh token is
// an HttpOnly cookie the browser sends automatically, so users never retype the
// password on their own machine.
let accessToken = sessionStorage.getItem('mytun-at') || ''
export function setAccessToken(t) {
  accessToken = t || ''
  if (t) sessionStorage.setItem('mytun-at', t)
  else sessionStorage.removeItem('mytun-at')
}
export function getAccessToken() { return accessToken }

let refreshing = null
async function refreshAccess() {
  if (!refreshing) {
    refreshing = (async () => {
      try {
        const r = await fetch('/api/refresh', { method: 'POST', credentials: 'same-origin' })
        if (!r.ok) throw new Error('refresh failed')
        const j = await r.json()
        if (j && j.accessToken) { setAccessToken(j.accessToken); return j.accessToken }
        throw new Error('no token')
      } finally {
        refreshing = null
      }
    })()
  }
  return refreshing
}

export class ApiError extends Error {
  constructor(message, status, body) {
    super(message)
    this.status = status
    this.body = body
  }
}

async function request(path, { method = 'GET', body, raw, retry = true } = {}) {
  const opts = { method, credentials: 'same-origin', headers: {} }
  if (accessToken) opts.headers.Authorization = `Bearer ${accessToken}`
  if (body !== undefined && !raw) {
    opts.headers = jsonHeaders
    opts.body = JSON.stringify(body)
  } else if (raw) {
    opts.body = body // NOT raw: raw is just the "do not JSON-encode" flag
  }
  const res = await fetch(path, opts)
  if (res.status === 401 && retry && !path.startsWith('/api/login') && !path.startsWith('/api/refresh')) {
    try {
      await refreshAccess()
      return request(path, { method, body, raw, retry: false })
    } catch { /* fall through to normal error handling */ }
  }
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
  login: async (username, password) => {
    const r = await request('/api/login', { method: 'POST', body: { username, password } })
    if (r && r.accessToken) setAccessToken(r.accessToken)
    return r
  },
  refresh: () => refreshAccess(),
  me: () => request('/api/config'),
  logout: async () => {
    try { await request('/api/logout', { method: 'POST' }) } catch { /* ignore */ }
    setAccessToken('')
  },
  health: () => request('/api/health'),
  config: () => request('/api/config'),
  saveConfig: (cfg) => request('/api/config', { method: 'POST', body: cfg }),
  status: () => request('/api/status'),
  consoleListen: () => request('/api/console/listen'),
  setConsoleListen: (body) => request('/api/console/listen', { method: 'POST', body }),
  apply: () => request('/api/apply', { method: 'POST' }),
  stopAll: () => request('/api/service/stop', { method: 'POST' }),
  changePassword: (payload) => request('/api/change-password', { method: 'POST', body: payload }),
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
  wsTicket: () => request('/api/ws-ticket'),
  exportAuditUrl: (limit = 100000) => `/api/traffic-logs/export?limit=${limit}`,
  trafficSeries: (minutes = 60) => request(`/api/stats/traffic?minutes=${minutes}`),
  speed: () => request('/api/speed'),
  revealSecret: (kind, id) => request(`/api/config/secret?kind=${kind}&id=${encodeURIComponent(id)}`),
}

// Live event stream. Reconnects with backoff and fans events out to handlers.
export function createEventStream(onEvent) {
  let ws = null
  let retry = 1000
  let closed = false
  // Browsers cannot set headers on the handshake, and putting the session token
  // in the URL leaks it into logs. Fetch a one-shot 30s ticket instead.
  async function ticket() {
    try {
      const r = await request('/api/ws-ticket')
      return r && r.ticket ? r.ticket : ''
    } catch {
      return ''
    }
  }
  const connect = async () => {
    if (closed) return
    const proto = location.protocol === 'https:' ? 'wss' : 'ws'
    const t = await ticket()
    ws = new WebSocket(`${proto}://${location.host}/ws${t ? '?ticket=' + encodeURIComponent(t) : ''}`)
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
