<template>
  <div class="flex flex-col gap-5">
    <div>
      <h2 class="text-xl font-semibold" style="color: var(--bt-text)">{{ t('overview.title') }}</h2>
      <p class="text-sm mt-1" style="color: var(--bt-text-secondary)">{{ t('overview.subtitle') }}</p>
    </div>

    <el-alert v-if="state.status.applyError" type="error" show-icon :closable="false" :title="t('overview.applyError')">
      {{ state.status.applyError }}
    </el-alert>

    <div class="grid grid-cols-1 md:grid-cols-2 xl:grid-cols-4 gap-4">
      <div v-for="card in cards" :key="card.label" class="bt-card p-5">
        <div class="flex items-start justify-between">
          <div>
            <div class="text-xs uppercase tracking-wider" style="color: var(--bt-text-muted)">{{ card.label }}</div>
            <div class="text-3xl font-semibold mt-2" style="color: var(--bt-text)">{{ card.value }}</div>
            <div class="text-xs mt-1" style="color: var(--bt-text-secondary)">{{ card.hint }}</div>
          </div>
          <div class="w-10 h-10 rounded-xl flex items-center justify-center" :style="{ background: 'var(--bt-primary-light)' }">
            <el-icon :size="20" color="var(--bt-primary)"><component :is="card.icon" /></el-icon>
          </div>
        </div>
      </div>
    </div>

    <div class="grid grid-cols-1 xl:grid-cols-3 gap-4">
      <div class="bt-card p-5 xl:col-span-1">
        <div class="font-medium mb-3" style="color: var(--bt-text)">{{ t('overview.quickActions') }}</div>
        <div class="flex flex-col gap-2">
          <el-button type="primary" :loading="busy === 'apply'" @click="applyAll">{{ t('overview.startAll') }}</el-button>
          <el-button plain :loading="busy === 'stop'" @click="stopAll">{{ t('overview.stopAll') }}</el-button>
          <el-button plain @click="clearLogs">{{ t('overview.clearLogs') }}</el-button>
        </div>
        <div class="mt-4 text-xs flex flex-col gap-1" style="color: var(--bt-text-secondary)">
          <div>{{ t('overview.platform') }}: <b>{{ state.systemProxy.platform || '—' }}</b></div>
          <div>{{ t('overview.dbPath') }}: <code>{{ state.status.dbPath || '—' }}</code></div>
          <div>{{ t('overview.sysproxyOn') }}: {{ state.status.systemProxy ? t('common.yes') : t('common.no') }}</div>
        </div>
      </div>

      <div class="bt-card p-5 xl:col-span-2">
        <div class="font-medium mb-3" style="color: var(--bt-text)">{{ t('overview.inboundClients') }}</div>
        <el-table :data="state.status.inbound || []" size="small" empty-text="—">
          <el-table-column prop="clientId" :label="t('config.clientId')" width="160" />
          <el-table-column prop="remoteAddr" :label="t('common.host')" min-width="160" />
          <el-table-column prop="channels" label="channels" width="100" />
          <el-table-column :label="t('common.status')" width="110">
            <template #default><el-tag type="success" size="small">online</el-tag></template>
          </el-table-column>
        </el-table>

        <div class="font-medium mt-5 mb-3" style="color: var(--bt-text)">{{ t('overview.outboundLinks') }}</div>
        <el-table :data="state.status.clients || []" size="small" empty-text="—">
          <el-table-column prop="alias" :label="t('common.name')" width="140" />
          <el-table-column label="endpoint" min-width="180">
            <template #default="{ row }">{{ row.host }}:{{ row.port }}</template>
          </el-table-column>
          <el-table-column :label="t('common.status')" width="130">
            <template #default="{ row }">
              <el-tag :type="row.status === 'connected' ? 'success' : row.status === 'failed' ? 'danger' : 'info'" size="small">
                {{ row.status }}
              </el-tag>
            </template>
          </el-table-column>
          <el-table-column prop="lastError" label="last error" min-width="180" show-overflow-tooltip />
        </el-table>
      </div>
    </div>

    <div class="bt-card p-5">
      <div class="flex items-center justify-between mb-3">
        <div class="font-medium" style="color: var(--bt-text)">近 60 分钟流量</div>
        <div class="text-xs" style="color: var(--bt-text-secondary)">
          连接 {{ chartTotals.conns }} · 下行 {{ fmtBytes(chartTotals.down) }} · 错误 {{ chartTotals.errors }}
        </div>
      </div>
      <div class="flex items-end gap-[2px]" style="height: 90px">
        <div v-for="(b, i) in trafficChart" :key="i" :title="new Date(b.ts).toLocaleTimeString() + ' · ' + fmtBytes(b.bytesDown)"
             :style="{ height: Math.max(2, b.height) + '%', background: b.errors ? 'var(--bt-danger)' : 'var(--bt-primary)', flex: 1, opacity: b.conns ? 0.85 : 0.2, borderRadius: '2px' }" />
      </div>
      <div class="flex justify-between text-xs mt-1" style="color: var(--bt-text-muted)">
        <span>-60m</span><span>now</span>
      </div>
    </div>

    <div class="bt-card p-5">
      <div class="flex items-center justify-between mb-3">
        <div class="font-medium" style="color: var(--bt-text)">{{ t('logs.title') }}</div>
        <el-button size="small" plain @click="$router.push('/logs')">{{ t('nav.logs') }}</el-button>
      </div>
      <el-table :data="recentAudit" size="small" empty-text="—">
        <el-table-column prop="module" :label="t('logs.module')" width="110" />
        <el-table-column prop="source" :label="t('logs.source')" min-width="150" />
        <el-table-column prop="target" :label="t('logs.target')" min-width="200" />
        <el-table-column :label="t('logs.bytes')" width="130">
          <template #default="{ row }">{{ fmtBytes(row.bytesUp + row.bytesDown) }}</template>
        </el-table-column>
        <el-table-column prop="status" :label="t('common.status')" width="90" />
      </el-table>
    </div>
  </div>
</template>

<script setup>
import { computed, onMounted, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { Service, Connection, Promotion, Switch } from '@element-plus/icons-vue'
import { api } from '../api'
import { t } from '../i18n'
import { useStore } from '../store'

const store = useStore()
const state = store.state
const busy = ref('')

const cards = computed(() => [
  { label: t('overview.serverRunning'), value: (state.status.servers || []).filter((s) => s.running).length, hint: `configured ${(state.config.servers || []).length}`, icon: Service },
  { label: t('overview.inboundClients'), value: (state.status.inbound || []).length, hint: `outbound ${(state.status.clients || []).filter((c) => c.status === 'connected').length}`, icon: Promotion },
  { label: t('overview.forwardRules'), value: (state.config.forwards || []).filter((f) => f.enabled).length, hint: `ports ${(state.status.forwardPorts?.server || []).join(', ') || '—'}`, icon: Connection },
  { label: t('overview.proxyRules'), value: (state.config.proxies || []).filter((p) => p.enabled).length, hint: `ports ${(state.status.proxyPorts?.server || []).join(', ') || '—'}`, icon: Switch },
])

const recentAudit = computed(() => (state.audit.length ? state.audit.slice(0, 8) : recent.value))

const recent = ref([])
const trafficChart = ref([])
const chartTotals = ref({ conns: 0, down: 0, errors: 0 })

async function loadChart() {
  try {
    const res = await api.trafficSeries(60)
    const buckets = res.buckets || []
    const max = Math.max(1, ...buckets.map(b => b.bytesDown || 0))
    trafficChart.value = buckets.map(b => ({
      ts: b.ts,
      conns: b.conns,
      errors: b.errors,
      height: Math.round(((b.bytesDown || 0) / max) * 100),
    }))
    chartTotals.value = {
      conns: buckets.reduce((a, b) => a + (b.conns || 0), 0),
      down: buckets.reduce((a, b) => a + (b.bytesDown || 0), 0),
      errors: buckets.reduce((a, b) => a + (b.errors || 0), 0),
    }
  } catch { /* ignore */ }
}

function fmtBytes(n) {
  const v = Number(n) || 0
  if (v < 1024) return `${v} B`
  if (v < 1048576) return `${(v / 1024).toFixed(1)} KB`
  if (v < 1073741824) return `${(v / 1048576).toFixed(1)} MB`
  return `${(v / 1073741824).toFixed(2)} GB`
}

async function applyAll() {
  busy.value = 'apply'
  try {
    const res = await api.apply()
    if (res.ok) ElMessage.success(t('common.saved'))
    else ElMessage.warning(res.error)
    await store.refreshStatus()
  } catch (e) {
    ElMessage.error(e.message)
  } finally {
    busy.value = ''
  }
}

async function stopAll() {
  busy.value = 'stop'
  try {
    await api.stopAll()
    await store.refreshStatus()
    ElMessage.success(t('common.saved'))
  } catch (e) {
    ElMessage.error(e.message)
  } finally {
    busy.value = ''
  }
}

async function clearLogs() {
  try {
    await api.clearLogs()
    recent.value = []
    state.audit = []
    ElMessage.success('审计日志已清空')
  } catch (e) {
    ElMessage.error(e.message)
  }
}

onMounted(async () => {
  await store.refreshStatus()
  await loadChart()
  try { recent.value = await api.trafficLogs({ limit: 8 }) } catch { /* ignore */ }
})
</script>
