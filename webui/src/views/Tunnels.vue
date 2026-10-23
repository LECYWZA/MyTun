<template>
  <div class="flex flex-col gap-5">
    <div class="flex items-start justify-between flex-wrap gap-3">
      <div>
        <h2 class="text-xl font-semibold" style="color: var(--bt-text)">{{ t('tunnels.title') }}</h2>
        <p class="text-sm mt-1" style="color: var(--bt-text-secondary)">{{ t('tunnels.subtitle') }}</p>
      </div>
      <div class="flex gap-2">
        <el-button plain :icon="Plus" @click="add">{{ t('tunnels.add') }}</el-button>
        <el-button type="primary" :loading="saving" @click="save">{{ t('common.save') }}</el-button>
      </div>
    </div>

    <div class="bt-card p-5">
      <el-table :data="state.config.forwards" size="default" empty-text="—">
        <el-table-column :label="t('tunnels.role')" width="110">
          <template #default="{ row }">
            <el-select v-model="row.role" size="small" @change="store.markDirty()">
              <el-option label="server" value="server" />
              <el-option label="client" value="client" />
            </el-select>
          </template>
        </el-table-column>
        <el-table-column :label="t('common.name')" min-width="120">
          <template #default="{ row }">
            <el-input v-model="row.note" size="small" :placeholder="t('common.note')" @input="store.markDirty()" />
          </template>
        </el-table-column>
        <el-table-column :label="t('tunnels.listenPort')" width="150">
          <template #default="{ row }">
            <el-input-number v-model="row.listenPort" :min="1" :max="65535" size="small" controls-position="right" class="!w-full" @change="onPortChange(row)" />
          </template>
        </el-table-column>
        <el-table-column :label="t('tunnels.targetHost')" min-width="150">
          <template #default="{ row }">
            <el-input v-model="row.targetHost" size="small" placeholder="127.0.0.1" @input="store.markDirty()" />
          </template>
        </el-table-column>
        <el-table-column :label="t('tunnels.targetPort')" width="130">
          <template #default="{ row }">
            <el-input-number v-model="row.targetPort" :min="1" :max="65535" size="small" controls-position="right" class="!w-full" @change="store.markDirty()" />
          </template>
        </el-table-column>
        <el-table-column :label="t('tunnels.targetClient')" width="170">
          <template #default="{ row }">
            <el-select v-model="row.targetClientId" size="small" clearable :placeholder="t('tunnels.autoTarget')" @change="store.markDirty()">
              <el-option v-for="c in clientOptions" :key="c.value" :label="c.label" :value="c.value" />
            </el-select>
          </template>
        </el-table-column>
        <el-table-column :label="t('common.status')" width="150">
          <template #default="{ row }">
            <div class="flex items-center gap-2">
              <el-switch v-model="row.enabled" size="small" @change="store.markDirty()" />
              <el-tag v-if="portState[row.listenPort] === false" type="danger" size="small">{{ t('tunnels.portInUse') }}</el-tag>
              <el-tag v-else-if="portState[row.listenPort] === true" type="success" size="small">{{ t('tunnels.portFree') }}</el-tag>
            </div>
          </template>
        </el-table-column>
        <el-table-column :label="t('common.actions')" width="80" align="center">
          <template #default="{ $index }">
            <el-button size="small" type="danger" plain :icon="Delete" @click="remove($index)" />
          </template>
        </el-table-column>
      </el-table>
    </div>

    <div class="bt-card p-5">
      <div class="font-medium mb-2" style="color: var(--bt-text)">live</div>
      <div class="text-sm flex flex-wrap gap-4" style="color: var(--bt-text-secondary)">
        <span>server ports: <code>{{ (state.status.forwardPorts?.server || []).join(', ') || '—' }}</code></span>
        <span>client ports: <code>{{ (state.status.forwardPorts?.client || []).join(', ') || '—' }}</code></span>
      </div>
    </div>
  </div>
</template>

<script setup>
import { computed, reactive, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { Delete, Plus } from '@element-plus/icons-vue'
import { api } from '../api'
import { t } from '../i18n'
import { useStore } from '../store'

const store = useStore()
const state = store.state
const saving = ref(false)
const portState = reactive({})

const clientOptions = computed(() => {
  const fromLinks = (state.config.clients || []).map((c) => ({ label: `${c.alias || c.host} (${c.id})`, value: c.id }))
  const fromInbound = (state.status.inbound || []).map((i) => ({ label: `在线: ${i.clientId}`, value: i.clientId }))
  return [...fromInbound, ...fromLinks]
})

function uid() {
  return Math.random().toString(36).slice(2, 10)
}

function add() {
  state.config.forwards.push({ id: uid(), role: 'server', listenHost: '0.0.0.0', listenPort: 8080, targetHost: '127.0.0.1', targetPort: 80, targetClientId: '', enabled: false, note: '' })
  store.markDirty()
}

function remove(idx) {
  state.config.forwards.splice(idx, 1)
  store.markDirty()
}

async function onPortChange(row) {
  store.markDirty()
  if (!row.listenPort) return
  try {
    const res = await api.checkPort(row.listenPort)
    portState[row.listenPort] = !!res.available
  } catch { /* ignore */ }
}

async function save() {
  saving.value = true
  try {
    const res = await store.save()
    if (res?.warning) ElMessage.warning(res.warning)
    else ElMessage.success(t('common.saved'))
  } catch (e) {
    ElMessage.error(e.message)
  } finally {
    saving.value = false
  }
}
</script>
