<template>
  <div class="flex flex-col gap-5">
    <div>
      <h2 class="text-xl font-semibold" style="color: var(--bt-text)">{{ t('tester.title') }}</h2>
      <p class="text-sm mt-1" style="color: var(--bt-text-secondary)">{{ t('tester.subtitle') }}</p>
    </div>

    <div class="bt-card p-5">
      <div class="grid grid-cols-1 md:grid-cols-3 gap-3">
        <el-form-item :label="t('tester.target')" class="!mb-0">
          <el-input v-model="form.target" placeholder="www.gstatic.com:443" />
        </el-form-item>
        <el-form-item :label="t('tester.viaRule')" class="!mb-0">
          <el-select v-model="form.ruleId" clearable :placeholder="t('tester.direct')">
            <el-option v-for="p in proxyOptions" :key="p.value" :label="p.label" :value="p.value" />
          </el-select>
        </el-form-item>
        <el-form-item :label="t('tester.downloadUrl')" class="!mb-0">
          <el-input v-model="form.url" />
        </el-form-item>
      </div>
      <div class="flex gap-2 mt-4">
        <el-button type="primary" :loading="running === 'lat'" @click="testLatency">{{ t('tester.latency') }}</el-button>
        <el-button plain :loading="running === 'speed'" @click="testSpeed">{{ t('tester.speed') }}</el-button>
      </div>
    </div>

    <div class="bt-card p-5">
      <div class="font-medium mb-3" style="color: var(--bt-text)">{{ t('tester.result') }}</div>
      <el-table :data="rows" size="small" empty-text="—">
        <el-table-column prop="time" :label="t('logs.time')" width="170" />
        <el-table-column prop="kind" :label="t('common.status')" width="110" />
        <el-table-column prop="via" :label="t('tester.viaRule')" min-width="140" />
        <el-table-column prop="result" :label="t('tester.result')" min-width="240" />
        <el-table-column :label="t('common.status')" width="90">
          <template #default="{ row }">
            <el-tag :type="row.ok ? 'success' : 'danger'" size="small">{{ row.ok ? 'ok' : 'fail' }}</el-tag>
          </template>
        </el-table-column>
      </el-table>
    </div>
  </div>
</template>

<script setup>
import { computed, reactive, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { api } from '../api'
import { t } from '../i18n'
import { useStore } from '../store'

const store = useStore()
const state = store.state
const running = ref('')
const rows = ref([])
const form = reactive({
  target: 'www.gstatic.com:443',
  ruleId: '',
  url: 'https://speed.cloudflare.com/__down?bytes=5000000',
})

const proxyOptions = computed(() =>
  (state.config.proxies || [])
    .filter((p) => p.enabled)
    .map((p) => ({
      label: `${p.listenHost === '0.0.0.0' ? '127.0.0.1' : p.listenHost}:${p.listenPort} (${p.networkMode})`,
      value: p.id,
    })),
)

function push(kind, via, ok, result) {
  rows.value.unshift({ time: new Date().toLocaleString(), kind, via: via || 'direct', ok, result })
  if (rows.value.length > 50) rows.value.length = 50
}

async function testLatency() {
  running.value = 'lat'
  try {
    const res = await api.testLatency({ target: form.target, ruleId: form.ruleId })
    const via = proxyOptions.value.find((p) => p.value === form.ruleId)?.label || ''
    if (res.ok) push('latency', via, true, t('tester.latencyResult', { ms: res.result.latency }))
    else push('latency', via, false, res.error)
  } catch (e) {
    ElMessage.error(e.message)
  } finally {
    running.value = ''
  }
}

async function testSpeed() {
  running.value = 'speed'
  try {
    const res = await api.testSpeed({ url: form.url, ruleId: form.ruleId })
    const via = proxyOptions.value.find((p) => p.value === form.ruleId)?.label || ''
    if (res.ok) {
      const r = res.result
      push('download', via, true, t('tester.speedResult', {
        mbps: r.mbps.toFixed(2), bytes: r.bytes, sec: (r.elapsedMs / 1000).toFixed(2),
      }))
    } else {
      push('download', via, false, res.error)
    }
  } catch (e) {
    ElMessage.error(e.message)
  } finally {
    running.value = ''
  }
}
</script>
