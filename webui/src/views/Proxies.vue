<template>
  <div class="flex flex-col gap-5">
    <div class="flex items-start justify-between flex-wrap gap-3">
      <div>
        <h2 class="text-xl font-semibold" style="color: var(--bt-text)">{{ t('proxies.title') }}</h2>
        <p class="text-sm mt-1" style="color: var(--bt-text-secondary)">{{ t('proxies.subtitle') }}</p>
      </div>
      <div class="flex gap-2">
        <el-button plain :icon="Plus" @click="add">{{ t('proxies.add') }}</el-button>
        <el-button type="primary" :loading="saving" @click="save">{{ t('common.save') }}</el-button>
      </div>
    </div>

    <div class="bt-card-grid">
      <div v-for="(p, idx) in state.config.proxies" :key="p.id" class="bt-card p-5">
        <div class="flex items-center justify-between mb-3">
          <div class="flex items-center gap-2">
            <el-tag :type="p.networkMode === 'remote' ? 'warning' : 'success'" size="small">
              {{ p.networkMode === 'remote' ? t('proxies.modeRemote') : t('proxies.modeLocal') }}
            </el-tag>
            <el-tag v-if="p.enabled" type="success" size="small" effect="plain">:{{ p.listenPort }}</el-tag>
          </div>
          <div class="flex items-center gap-2">
            <el-switch v-model="p.enabled" size="small" @change="store.markDirty()" />
            <el-button size="small" type="danger" plain :icon="Delete" @click="remove(idx)" />
          </div>
        </div>

        <div class="grid grid-cols-2 gap-3">
          <el-form-item :label="t('proxies.mode')" class="!mb-0">
            <el-select v-model="p.networkMode" size="default" @change="store.markDirty()">
              <el-option :label="t('proxies.modeLocal')" value="local" />
              <el-option :label="t('proxies.modeRemote')" value="remote" />
            </el-select>
          </el-form-item>
          <el-form-item :label="t('common.port')" class="!mb-0">
            <el-input-number v-model="p.listenPort" :min="1" :max="65535" controls-position="right" class="!w-full" @change="store.markDirty()" />
          </el-form-item>
          <el-form-item :label="t('config.bindHost')" class="!mb-0">
            <el-input v-model="p.listenHost" @input="store.markDirty()" />
          </el-form-item>
          <el-form-item v-if="p.networkMode === 'remote'" :label="t('tunnels.targetClient')" class="!mb-0">
            <el-select v-model="p.targetClientId" clearable :placeholder="t('tunnels.autoTarget')" @change="store.markDirty()">
              <el-option v-for="c in clientOptions" :key="c.value" :label="c.label" :value="c.value" />
            </el-select>
          </el-form-item>
          <el-form-item :label="t('proxies.auth')" class="!mb-0">
            <el-switch v-model="p.useAuth" @change="store.markDirty()" />
          </el-form-item>
          <el-form-item :label="t('common.note')" class="!mb-0">
            <el-input v-model="p.note" @input="store.markDirty()" />
          </el-form-item>
        </div>

        <div v-if="p.useAuth" class="grid grid-cols-2 gap-3 mt-2">
          <el-form-item :label="t('proxies.user')" class="!mb-0">
            <el-input v-model="p.user" @input="store.markDirty()" />
          </el-form-item>
          <el-form-item :label="t('proxies.pass')" class="!mb-0">
            <el-input v-model="p.pass" show-password @focus="reveal(p)" @input="store.markDirty()" />
          </el-form-item>
        </div>

        <div class="mt-3 text-xs" style="color: var(--bt-text-secondary)">
          <div class="mb-1">{{ t('proxies.endpoint') }}:
            <code>http://{{ p.listenHost === '0.0.0.0' ? '127.0.0.1' : p.listenHost }}:{{ p.listenPort }}</code>
            <el-button size="small" text @click="copy(`http://${p.listenHost === '0.0.0.0' ? '127.0.0.1' : p.listenHost}:${p.listenPort}`)">{{ t('common.copy') }}</el-button>
          </div>
          <div>{{ t('proxies.allowLocalBypass') }}</div>
        </div>

        <el-collapse class="mt-2">
          <el-collapse-item :title="t('proxies.allowIps')">
            <div class="grid grid-cols-1 gap-2">
              <el-input v-model="p.allowIps" type="textarea" :rows="2" placeholder="127.0.0.1, 192.168.1.0/24" @input="store.markDirty()" />
              <el-input v-model="p.denyIps" type="textarea" :rows="2" :placeholder="t('proxies.denyIps')" @input="store.markDirty()" />
            </div>
          </el-collapse-item>
        </el-collapse>
      </div>
    </div>

    <el-empty v-if="!state.config.proxies.length" :description="t('common.none')" :image-size="70" />

    <div class="bt-card p-5">
      <div class="text-sm flex flex-wrap gap-4" style="color: var(--bt-text-secondary)">
        <span>live ports: <code>{{ (state.status.proxyPorts?.server || []).join(', ') || '—' }}</code></span>
        <span>connections: <b>{{ state.status.proxyConns || 0 }}</b></span>
      </div>
    </div>
  </div>
</template>

<script setup>
import { computed, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { Delete, Plus } from '@element-plus/icons-vue'
import { t } from '../i18n'
import { useStore } from '../store'

const store = useStore()

async function reveal(row) {
  if (row.pass !== MASK || !row.id) return
  try {
    const r = await api.revealSecret('proxy', row.id)
    if (r && typeof r.password === 'string') row.pass = r.password
  } catch { /* keep the mask */ }
}
const state = store.state
const saving = ref(false)

const clientOptions = computed(() => {
  const fromLinks = (state.config.clients || []).map((c) => ({ label: `${c.alias || c.host} (${c.id})`, value: c.id }))
  const fromInbound = (state.status.inbound || []).map((i) => ({ label: `在线: ${i.clientId}`, value: i.clientId }))
  return [...fromInbound, ...fromLinks]
})

function uid() {
  return Math.random().toString(36).slice(2, 10)
}

function add() {
  state.config.proxies.push({
    id: uid(), role: 'server', listenHost: '0.0.0.0', listenPort: 1080,
    networkMode: 'local', targetClientId: '', useAuth: false, user: '', pass: '',
    allowIps: '', denyIps: '', enabled: false, note: '',
  })
  store.markDirty()
}

function remove(idx) {
  state.config.proxies.splice(idx, 1)
  store.markDirty()
}

async function copy(text) {
  try {
    await navigator.clipboard.writeText(text)
    ElMessage.success(t('common.copied'))
  } catch {
    ElMessage.info(text)
  }
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
