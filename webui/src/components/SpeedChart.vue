<template>
  <div class="bt-speed-chart">
    <div class="flex items-baseline justify-between mb-1">
      <span class="text-xs" style="color: var(--bt-text-secondary)">{{ label }}</span>
      <span class="text-xs" style="color: var(--bt-text-muted)">{{ fmtRate(peakValue) }}</span>
    </div>
    <svg :viewBox="`0 0 ${W} ${H}`" preserveAspectRatio="none" class="bt-speed-svg" :style="{ height: height + 'px' }">
      <defs>
        <linearGradient :id="gid + '-d'" x1="0" y1="0" x2="0" y2="1">
          <stop offset="0%" stop-color="var(--bt-primary)" stop-opacity="0.28" />
          <stop offset="100%" stop-color="var(--bt-primary)" stop-opacity="0" />
        </linearGradient>
      </defs>
      <path :d="areaPath" :fill="`url(#${gid}-d)`" />
      <path :d="upPath" stroke="var(--bt-success)" fill="none" stroke-width="1.3" stroke-linejoin="round" stroke-linecap="round" opacity="0.85" />
      <path :d="downPath" stroke="var(--bt-primary)" fill="none" stroke-width="1.5" stroke-linejoin="round" stroke-linecap="round" />
    </svg>
  </div>
</template>

<script setup>
import { computed, onBeforeUnmount, ref, watch } from 'vue'

const props = defineProps({
  up: { type: Array, default: () => [] },
  down: { type: Array, default: () => [] },
  label: { type: String, default: '' },
  peak: { type: Number, default: 0 },
  height: { type: Number, default: 52 },
})

const W = 300
const H = 52
const N = 60
const gid = `sg-${Math.random().toString(36).slice(2, 8)}`

// The chart never jumps straight to a new sample: it eases the drawn shape
// toward fresh data every frame, which is what makes the movement read as a
// fluid slide instead of hard steps.
const shownUp = ref([])
const shownDown = ref([])
let targetUp = []
let targetDown = []
let raf = 0
let running = false

const peakValue = computed(() => {
  let m = props.peak || 0
  for (const v of targetUp) if (v > m) m = v
  for (const v of targetDown) if (v > m) m = v
  return m
})

function norm(v) {
  const p = peakValue.value
  if (!p || p <= 0) return 0
  return Math.min(1, v / p)
}

function resample(arr, n) {
  if (arr.length === n) return arr
  const out = new Array(n).fill(0)
  for (let i = 0; i < n; i++) out[i] = arr[arr.length - n + i] ?? 0
  return out
}

function step() {
  const cu = resample(shownUp.value, N)
  const cd = resample(shownDown.value, N)
  const tu = resample(targetUp, N)
  const td = resample(targetDown, N)
  let moved = false
  const nu = new Array(N)
  const nd = new Array(N)
  for (let i = 0; i < N; i++) {
    const du = tu[i] - cu[i]
    const dd = td[i] - cd[i]
    if (Math.abs(du) > 0.0005 || Math.abs(dd) > 0.0005) moved = true
    nu[i] = cu[i] + du * 0.12
    nd[i] = cd[i] + dd * 0.12
  }
  shownUp.value = nu
  shownDown.value = nd
  if (moved) {
    raf = requestAnimationFrame(step)
  } else {
    running = false
    raf = 0
  }
}

function kick() {
  if (running) return
  running = true
  raf = requestAnimationFrame(step)
}

watch(
  () => [props.up, props.down],
  () => {
    targetUp = (props.up || []).map(norm)
    targetDown = (props.down || []).map(norm)
    if (!shownUp.value.length) {
      shownUp.value = resample(targetUp, N)
      shownDown.value = resample(targetDown, N)
    }
    kick()
  },
  { deep: true, immediate: true },
)

watch(peakValue, () => kick())

onBeforeUnmount(() => {
  if (raf) cancelAnimationFrame(raf)
})

function points(arr) {
  const n = arr.length || 1
  const stepX = W / Math.max(1, n - 1)
  return arr.map((v, i) => ({ x: i * stepX, y: H - 2 - v * (H - 4) }))
}

// Catmull-Rom -> cubic bezier: soft curve instead of jagged segments.
function smoothPath(pts) {
  if (pts.length < 2) return ''
  let d = `M ${pts[0].x.toFixed(2)} ${pts[0].y.toFixed(2)}`
  for (let i = 0; i < pts.length - 1; i++) {
    const p0 = pts[i - 1] || pts[i]
    const p1 = pts[i]
    const p2 = pts[i + 1]
    const p3 = pts[i + 2] || p2
    const c1x = p1.x + (p2.x - p0.x) / 6
    const c1y = p1.y + (p2.y - p0.y) / 6
    const c2x = p2.x - (p3.x - p1.x) / 6
    const c2y = p2.y - (p3.y - p1.y) / 6
    d += ` C ${c1x.toFixed(2)} ${c1y.toFixed(2)}, ${c2x.toFixed(2)} ${c2y.toFixed(2)}, ${p2.x.toFixed(2)} ${p2.y.toFixed(2)}`
  }
  return d
}

const downPath = computed(() => smoothPath(points(resample(shownDown.value, N))))
const upPath = computed(() => smoothPath(points(resample(shownUp.value, N))))
const areaPath = computed(() => {
  const pts = points(resample(shownDown.value, N))
  if (pts.length < 2) return ''
  return `${smoothPath(pts)} L ${W} ${H} L 0 ${H} Z`
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
</script>

<style scoped>
.bt-speed-svg {
  width: 100%;
  display: block;
  overflow: visible;
}
</style>
