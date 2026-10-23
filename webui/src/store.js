// Shared console state: configuration, live status and the websocket feed.
import { computed, reactive, ref } from 'vue'
import { api, createEventStream } from './api'

const state = reactive({
  authenticated: false,
  loading: false,
  config: {
    servers: [],
    clients: [],
    forwards: [],
    proxies: [],
    settings: {},
  },
  status: {
    servers: [],
    inbound: [],
    clients: [],
    forwardPorts: { server: [], client: [] },
    proxyPorts: { server: [], client: [] },
    systemProxy: false,
    applyError: '',
  },
  systemProxy: { enabled: false, current: '', residue: false, detail: '', supported: true, platform: '' },
  audit: [],
  chat: { messages: [], files: [] },
  dirty: false,
  wsConnected: false,
})

let stream = null
const listeners = new Set()

export function onEvent(fn) {
  listeners.add(fn)
  return () => listeners.delete(fn)
}

function dispatch(evt) {
  for (const fn of listeners) {
    try { fn(evt) } catch { /* listener errors must not break the feed */ }
  }
}

export function useStore() {
  const inboundCount = computed(() => (state.status.inbound || []).length)
  const linkedCount = computed(() => (state.status.clients || []).filter((c) => c.status === 'connected').length)
  const forwardCount = computed(() => (state.config.forwards || []).filter((f) => f.enabled).length)
  const proxyCount = computed(() => (state.config.proxies || []).filter((p) => p.enabled).length)

  async function bootstrap() {
    state.loading = true
    try {
      const cfg = await api.config()
      applyConfig(cfg)
      state.authenticated = true
      await refreshStatus()
      await refreshSystemProxy()
      startStream()
    } catch (e) {
      if (e.status === 401) state.authenticated = false
      else throw e
    } finally {
      state.loading = false
    }
  }

  function applyConfig(cfg) {
    if (!cfg) return
    state.config = {
      servers: cfg.servers || [],
      clients: cfg.clients || [],
      forwards: cfg.forwards || [],
      proxies: cfg.proxies || [],
      settings: cfg.settings || {},
    }
    state.dirty = false
  }

  async function refreshStatus() {
    try {
      state.status = await api.status()
    } catch { /* keep last known status */ }
  }

  async function refreshSystemProxy() {
    try {
      state.systemProxy = await api.systemProxy('status')
    } catch { /* headless hosts may not support it */ }
  }

  async function save() {
    const payload = JSON.parse(JSON.stringify(state.config))
    const res = await api.saveConfig(payload)
    if (res?.config) applyConfig(res.config)
    state.dirty = false
    await refreshStatus()
    return res
  }

  function markDirty() { state.dirty = true }

  async function addRow(kind, row) {
    state.config[kind].push(row)
    markDirty()
    return save()
  }

  async function removeRow(kind, id) {
    state.config[kind] = state.config[kind].filter((r) => r.id !== id)
    markDirty()
    return save()
  }

  function startStream() {
    if (stream) return
    stream = createEventStream((msg) => {
      if (msg.event === 'status') {
        state.wsConnected = true
        state.status = msg.data
      } else if (msg.event === 'audit') {
        state.audit.unshift(msg.data)
        if (state.audit.length > 500) state.audit.length = 500
      } else if (msg.event === 'chat_message' || msg.event === 'chat_file') {
        if (state.chat.messages) state.chat.messages.push(msg.data)
        if (msg.event === 'chat_file' && state.chat.files) state.chat.files.push(msg.data)
      } else if (msg.event === 'apply_error') {
        state.status.applyError = msg.data
      }
      dispatch(msg)
    })
  }

  function stopStream() {
    if (stream) { stream.close(); stream = null }
    state.wsConnected = false
  }

  async function logout() {
    try { await api.logout() } catch { /* ignore */ }
    state.authenticated = false
    stopStream()
  }

  return {
    state, inboundCount, linkedCount, forwardCount, proxyCount,
    bootstrap, refreshStatus, refreshSystemProxy, save, markDirty,
    addRow, removeRow, setAuthenticated: (v) => { state.authenticated = v },
    startStream, stopStream, logout,
  }
}

export { state }
