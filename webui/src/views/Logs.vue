<template>
  <div class="flex flex-col gap-5">
    <div class="flex items-start justify-between flex-wrap gap-3">
      <div>
        <h2 class="text-xl font-semibold" style="color: var(--bt-text)">{{ t('logs.title') }}</h2>
        <p class="text-sm mt-1" style="color: var(--bt-text-secondary)">{{ t('logs.subtitle') }}</p>
      </div>
      <div class="flex gap-2 items-center">
        <el-tag type="info" size="small">
          保留 {{ retainDays }} 天 / 上限 {{ maxRows }} 条 · 当前 {{ audit.rows || 0 }} 条 · {{ ((audit.dbBytes || 0) / 1048576).toFixed(1) }} MB
        </el-tag>
        <el-button plain size="small" @click="showPolicy = true">轮转设置</el-button>
        <el-tag :type="live ? 'success' : 'info'" size="small">{{ live ? t('logs.live') : t('logs.paused') }}</el-tag>
        <el-button plain size="small" @click="live = !live">{{ live ? t('logs.paused') : t('logs.live') }}</el-button>
        <el-button plain size="small" :icon="Refresh" @click="load">{{ t('common.refresh') }}</el-button>
        <el-button type="danger" plain size="small" :icon="Delete" @click="clear">{{ t('logs.clear') }}</el-button>
      </div>
    </div>

    <div class="bt-card p-4">
      <div class="grid grid-cols-1 md:grid-cols-4 gap-3">
        <el-select v-model="filter.module" clearable :placeholder="t('logs.filterModule')" @change="load">
          <el-option label="http" value="http" />
          <el-option label="socks5" value="socks5" />
          <el-option label="forward" value="forward" />
        </el-select>
        <el-select v-model="filter.status" clearable :placeholder="t('logs.filterStatus')" @change="load">
          <el-option label="success" value="success" />
          <el-option label="failed" value="failed" />
          <el-option label="blocked" value="blocked" />
        </el-select>
        <el-input v-model="filter.target" clearable :placeholder="t('logs.filterTarget')" @keyup.enter="load" />
        <el-button plain @click="load">{{ t('common.refresh') }}</el-button>
      </div>
    </div>

    <el-dialog v-model="showPolicy" title="审计日志轮转" width="420px">
      <el-form label-position="top">
        <el-form-item label="最多保留条数（超出后删除最旧）">
          <el-input-number v-model="maxRows" :min="1000" :max="1000000" step="1000" controls-position="right" class="!w-full" />
        </el-form-item>
        <el-form-item label="最长保留天数（0 = 不按时间删）">
          <el-input-number v-model="retainDays" :min="0" :max="3650" controls-position="right" class="!w-full" />
        </el-form-item>
        <div class="text-xs" style="color: var(--bt-text-secondary)">
          每 10 分钟自动按以上策略轮转：先删超期记录，再按条数上限裁剪；删除后自动 CHECKPOINT 并在大批量删除时 VACUUM 收缩文件。
        </div>
      </el-form>
      <template #footer>
        <el-button @click="showPolicy = false">取消</el-button>
        <el-button type="primary" :loading="policySaving" @click="savePolicy">保存并立即轮转</el-button>
      </template>
    </el-dialog>

    <div class="bt-card p-4">
      <el-table :data="rows" size="small" height="560" empty-text="—">
        <el-table-column :label="t('logs.time')" width="160">
          <template #default="{ row }">{{ fmtTime(row.ts) }}</template>
        </el-table-column>
        <el-table-column prop="module" :label="t('logs.module')" width="100" />
        <el-table-column prop="source" :label="t('logs.source')" width="160" show-overflow-tooltip />
        <el-table-column prop="target" :label="t('logs.target')" min-width="200" show-overflow-tooltip />
        <el-table-column prop="action" :label="t('logs.action')" width="100" />
        <el-table-column :label="t('logs.bytes')" width="150">
          <template #default="{ row }">↑{{ fmtBytes(row.bytesUp) }} ↓{{ fmtBytes(row.bytesDown) }}</template>
        </el-table-column>
        <el-table-column :label="t('logs.duration')" width="90">
          <template #default="{ row }">{{ row.durationMs }}ms</template>
        </el-table-column>
        <el-table-column :label="t('common.status')" width="90">
          <template #default="{ row }">
            <el-tag :type="row.status === 'success' ? 'success' : row.status === 'blocked' ? 'warning' : 'danger'" size="small">
              {{ row.status }}
            </el-tag>
          </template>
        </el-table-column>
        <el-table-column prop="error" label="error" min-width="160" show-overflow-tooltip />
      </el-table>
    </div>
  </div>
</template>

<script setup>
import { onMounted, onUnmounted, reactive, ref, watch } from 'vue'
import { ElMessage } from 'element-plus'
import { Delete, Refresh } from '@element-plus/icons-vue'
import { api } from '../api'
import { t } from '../i18n'
import { onEvent, useStore } from '../store'

const store = useStore()
const state = store.state
const rows = ref([])
const live = ref(true)
const filter = reactive({ module: '', status: '', target: '' })
const audit = ref({})
const showPolicy = ref(false)
const policySaving = ref(false)
const maxRows = ref(50000)
const retainDays = ref(7)

async function loadAuditStats() {
  try {
    const st = await api.status()
    audit.value = st.audit || {}
    if (st.auditPolicy) {
      maxRows.value = Number(st.auditPolicy.maxRows || maxRows.value)
      retainDays.value = Number(st.auditPolicy.retainDays || retainDays.value)
    }
  } catch { /* ignore */ }
}

async function savePolicy() {
  policySaving.value = true
  try {
    await api.settings({ audit_max_rows: String(maxRows.value), audit_retain_days: String(retainDays.value) })
    const res = await api.pruneLogs({ keep: maxRows.value, days: retainDays.value })
    ElMessage.success(`已轮转，删除 ${res.removed || 0} 条`)
    showPolicy.value = false
    await load()
    await loadAuditStats()
  } catch (e) {
    ElMessage.error(e.message)
  } finally {
    policySaving.value = false
  }
}

function fmtTime(ts) {
  if (!ts) return '—'
  return new Date(ts).toLocaleString()
}

function fmtBytes(n) {
  const v = Number(n) || 0
  if (v < 1024) return `${v}B`
  if (v < 1048576) return `${(v / 1024).toFixed(0)}K`
  if (v < 1073741824) return `${(v / 1048576).toFixed(1)}M`
  return `${(v / 1073741824).toFixed(2)}G`
}

async function load() {
  try {
    rows.value = await api.trafficLogs({ ...filter, limit: 400 })
  } catch (e) {
    ElMessage.error(e.message)
  }
}

async function clear() {
  try {
    await api.clearLogs()
    rows.value = []
    state.audit = []
    ElMessage.success(t('common.saved'))
  } catch (e) {
    ElMessage.error(e.message)
  }
}

let off = null
onMounted(async () => {
  await load()
  await loadAuditStats()
  off = onEvent((msg) => {
    if (msg.event !== 'audit' || !live.value) return
    const rec = msg.data
    if (filter.module && rec.module !== filter.module) return
    if (filter.status && rec.status !== filter.status) return
    if (filter.target && !String(rec.target || '').includes(filter.target)) return
    rows.value.unshift(rec)
    if (rows.value.length > 600) rows.value.length = 600
  })
})

onUnmounted(() => { if (off) off() })
watch(() => filter.module + filter.status, () => load())
</script>
