<template>
  <aside class="bt-rail">
    <div class="bt-rail-card">
      <div class="flex items-center justify-between mb-3">
        <div class="font-medium" style="color: var(--bt-text); font-size: 13px">{{ t('rail.title') }}</div>
        <span class="bt-rail-live">
          <span class="bt-dot" :class="live ? 'bt-dot-active' : 'bt-dot-inactive'"></span>
          {{ live ? t('rail.live') : t('rail.idle') }}
        </span>
      </div>

      <div class="bt-rail-speed">
        <div class="bt-rail-metric">
          <span class="bt-rail-arrow" style="color: var(--bt-success)">↑</span>
          <div class="flex flex-col leading-none">
            <span class="bt-rail-value" style="color: var(--bt-success)">{{ fmtRate(shownUp) }}</span>
            <span class="bt-rail-label">{{ t('rail.up') }}</span>
          </div>
        </div>
        <div class="bt-rail-metric">
          <span class="bt-rail-arrow" style="color: var(--bt-primary)">↓</span>
          <div class="flex flex-col leading-none">
            <span class="bt-rail-value" style="color: var(--bt-primary)">{{ fmtRate(shownDown) }}</span>
            <span class="bt-rail-label">{{ t('rail.down') }}</span>
          </div>
        </div>
      </div>

      <div class="flex flex-col gap-3 mt-1">
        <SpeedChart
          v-for="w in windows"
          :key="w.key"
          :up="series[w.key].up"
          :down="series[w.key].down"
          :peak="series[w.key].peak"
          :label="w.label"
          :height="52"
        />
      </div>

      <div class="bt-rail-foot">
        <div>{{ t('rail.total') }}</div>
        <div class="flex gap-3 mt-1">
          <span>↑ <b>{{ fmtBytes(totals.up) }}</b></span>
          <span>↓ <b>{{ fmtBytes(totals.down) }}</b></span>
        </div>
      </div>
    </div>
  </aside>
</template>

<script setup>
import { computed, onBeforeUnmount, onMounted, reactive, ref } from 'vue'
import SpeedChart from './SpeedChart.vue'
import { api } from '../api'
import { t } from '../i18n'

const POLL_MS = 2000
const series = reactive({
  m5: { up: [], down: [], peak: 0 },
  m15: { up: [], down: [], peak: 0 },
  m30: { up: [], down: [], peak: 0 },
})
const totals = reactive({ up: 0, down: 0 })
const live = ref(false)

const windows = computed(() => [
  { key: 'm5', label: t('rail.min5') },
  { key: 'm15', label: t('rail.min15') },
  { key: 'm30', label: t('rail.min30') },
])

// Displayed numbers ease toward the sampled speed instead of snapping, so the
// readout behaves like a real gauge rather than flickering on every poll.
const shownUp = ref(0)
const shownDown = ref(0)
let targetUp = 0
let targetDown = 0
let raf = 0
let timer = 0

function tick() {
  const du = targetUp - shownUp.value
  const dd = targetDown - shownDown.value
  shownUp.value += Math.abs(du) < 1 ? du : du * 0.09
  shownDown.value += Math.abs(dd) < 1 ? dd : dd * 0.09
  if (Math.abs(du) > 1 || Math.abs(dd) > 1) {
    raf = requestAnimationFrame(tick)
  } else {
    raf = 0
  }
}

function kickNumbers() {
  if (!raf) raf = requestAnimationFrame(tick)
}

async function poll() {
  try {
    const d = await api.speed()
    targetUp = d.up || 0
    targetDown = d.down || 0
    live.value = true
    if (d.m5) Object.assign(series.m5, d.m5)
    if (d.m15) Object.assign(series.m15, d.m15)
    if (d.m30) Object.assign(series.m30, d.m30)
    totals.up = d.totalUp || 0
    totals.down = d.totalDown || 0
    kickNumbers()
  } catch {
    live.value = false
  }
}

function start() {
  if (timer) return
  poll()
  timer = setInterval(poll, POLL_MS)
}

function stop() {
  if (timer) {
    clearInterval(timer)
    timer = 0
  }
}

function onVisibility() {
  if (document.hidden) stop()
  else start()
}

onMounted(() => {
  start()
  document.addEventListener('visibilitychange', onVisibility)
})

onBeforeUnmount(() => {
  stop()
  document.removeEventListener('visibilitychange', onVisibility)
  if (raf) cancelAnimationFrame(raf)
})

function fmtRate(v) {
  if (!v || v < 1) return '0 B/s'
  const u = ['B/s', 'KB/s', 'MB/s', 'GB/s']
  let i = 0
  let n = v
  while (n >= 1024 && i < u.length - 1) {
    n /= 1024
    i++
  }
  return `${n.toFixed(n >= 100 ? 0 : 1)} ${u[i]}`
}

function fmtBytes(v) {
  if (!v || v < 1) return '0 B'
  const u = ['B', 'KB', 'MB', 'GB', 'TB']
  let i = 0
  let n = v
  while (n >= 1024 && i < u.length - 1) {
    n /= 1024
    i++
  }
  return `${n.toFixed(n >= 100 ? 0 : 1)} ${u[i]}`
}
</script>
