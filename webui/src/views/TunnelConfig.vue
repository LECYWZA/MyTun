<template>
  <div class="flex flex-col gap-5">
    <div class="flex items-start justify-between flex-wrap gap-3">
      <div>
        <h2 class="text-xl font-semibold" style="color: var(--bt-text)">{{ t('config.title') }}</h2>
        <p class="text-sm mt-1" style="color: var(--bt-text-secondary)">{{ t('config.subtitle') }}</p>
      </div>
      <div class="flex gap-2">
        <el-button plain :loading="saving" @click="exportConfig">{{ t('config.export') }}</el-button>
        <el-button plain @click="pickImport">{{ t('config.import') }}</el-button>
        <el-button type="primary" :loading="saving" @click="save">{{ t('common.save') }}</el-button>
      </div>
      <input ref="importInput" type="file" accept="application/json" class="hidden" @change="doImport" />
    </div>

    <el-alert type="info" :closable="false" show-icon :title="t('config.hint')" />

    <div class="bt-card p-5">
      <div class="flex items-center justify-between mb-3">
        <div class="font-medium" style="color: var(--bt-text)">{{ t('config.serverSection') }}</div>
        <el-button size="small" plain :icon="Plus" @click="addServer">{{ t('config.addServer') }}</el-button>
      </div>
      <div v-for="(s, idx) in state.config.servers" :key="s.id" class="rounded-xl p-4 mb-3" style="border: 1px solid var(--bt-border)">
        <div class="bt-param-row">
          <el-form-item :label="t('common.name')" class="!mb-0">
            <el-input v-model="s.alias" style="width: 150px" @input="store.markDirty()" />
          </el-form-item>
          <el-form-item :label="t('config.bindHost')" class="!mb-0">
            <el-input v-model="s.bindHost" style="width: 160px" @input="store.markDirty()" />
          </el-form-item>
          <el-form-item :label="t('config.tunnelPort')" class="!mb-0">
            <el-input-number v-model="s.port" style="width: 130px" :min="1" :max="65535" controls-position="right" class="!w-full" @change="store.markDirty()" />
          </el-form-item>
          <el-form-item :label="t('common.password')" class="!mb-0">
            <el-input v-model="s.password" style="width: 170px" show-password @focus="reveal(s, 'server')" @input="store.markDirty()" />
          </el-form-item>
          <el-form-item :label="t('config.commonName')" class="!mb-0">
            <el-input v-model="s.commonName" style="width: 190px" placeholder="mail.qq.com" @input="store.markDirty()" />
          </el-form-item>
          <div class="flex items-end gap-2">
            <el-switch v-model="s.enabled" @change="store.markDirty()" />
            <el-button size="small" type="danger" plain :icon="Delete" @click="removeServer(idx)" />
          </div>
        </div>
      </div>
      <el-empty v-if="!state.config.servers.length" :description="t('common.none')" :image-size="60" />
    </div>

    <div class="bt-card p-5">
      <div class="flex items-center justify-between mb-3">
        <div class="font-medium" style="color: var(--bt-text)">{{ t('config.clientSection') }}</div>
        <el-button size="small" plain :icon="Plus" @click="addClient">{{ t('config.addClient') }}</el-button>
      </div>
      <div v-for="(c, idx) in state.config.clients" :key="c.id" class="rounded-xl p-4 mb-3" style="border: 1px solid var(--bt-border)">
        <div class="bt-param-row">
          <el-form-item :label="t('common.name')" class="!mb-0">
            <el-input v-model="c.alias" style="width: 150px" @input="store.markDirty()" />
          </el-form-item>
          <el-form-item :label="t('config.upstreamHost')" class="!mb-0">
            <el-input v-model="c.host" style="width: 200px" placeholder="example.com" @input="store.markDirty()" />
          </el-form-item>
          <el-form-item :label="t('config.tunnelPort')" class="!mb-0">
            <el-input-number v-model="c.port" style="width: 130px" :min="1" :max="65535" controls-position="right" class="!w-full" @change="store.markDirty()" />
          </el-form-item>
          <el-form-item :label="t('common.password')" class="!mb-0">
            <el-input v-model="c.password" style="width: 170px" show-password @focus="reveal(c, 'client')" @input="store.markDirty()" />
          </el-form-item>
          <el-form-item :label="t('config.clientId')" class="!mb-0">
            <el-input v-model="c.clientId" style="width: 140px" @input="store.markDirty()" />
          </el-form-item>
          <el-form-item :label="t('config.commonName')" class="!mb-0">
            <el-input v-model="c.sni" style="width: 190px" placeholder="mail.qq.com" @input="store.markDirty()" />
          </el-form-item>
          <div class="flex items-end gap-2">
            <el-switch v-model="c.enabled" @change="store.markDirty()" />
            <el-button size="small" type="danger" plain :icon="Delete" @click="removeClient(idx)" />
          </div>
        </div>
      </div>
      <el-empty v-if="!state.config.clients.length" :description="t('common.none')" :image-size="60" />
    </div>

    <div class="bt-card p-5">
      <div class="flex items-start justify-between flex-wrap gap-3">
        <div>
          <div class="font-medium" style="color: var(--bt-text)">{{ t('qos.title') }}</div>
          <div class="text-xs mt-1" style="color: var(--bt-text-secondary)">
            {{ t('qos.subtitle') }}
          </div>
        </div>
        <el-switch v-model="qos.enabled" :loading="qosSaving" @change="saveQos" />
      </div>

      <div class="grid grid-cols-1 md:grid-cols-4 gap-3 mt-4">
        <el-form-item :label="t('qos.mode')" class="!mb-0">
          <el-select v-model="qos.mode" :disabled="!qos.enabled" @change="saveQos">
            <el-option :label="t('qos.modeAuto')" value="auto" />
            <el-option :label="t('qos.modeFixed')" value="fixed" />
          </el-select>
        </el-form-item>
        <el-form-item :label="t('qos.cap')" class="!mb-0">
          <el-input-number v-model="qos.totalMbps" :min="0" :max="100000" :disabled="!qos.enabled" controls-position="right" class="!w-full" @change="saveQos" />
        </el-form-item>
        <el-form-item :label="t('qos.interactivePct')" class="!mb-0">
          <el-input-number v-model="qos.interactivePercent" :min="5" :max="90" :disabled="!qos.enabled" controls-position="right" class="!w-full" @change="saveQos" />
        </el-form-item>
        <el-form-item :label="t('qos.floor')" class="!mb-0">
          <el-input-number v-model="qos.interactiveFloorKbps" :min="64" :max="100000" :disabled="!qos.enabled" controls-position="right" class="!w-full" @change="saveQos" />
        </el-form-item>
      </div>

      <div v-if="qos.enabled" class="mt-3">
        <div class="text-xs flex flex-wrap gap-4" style="color: var(--bt-text-secondary)">
          <span>{{ t('qos.capacity') }}: <b>{{ (state.status.qos?.capacityKbps || 0) }} Kbps</b></span>
          <span>{{ t('qos.observed') }}: <b>{{ (state.status.qos?.observedKbps || 0) }} Kbps</b></span>
          <span>{{ t('qos.streams') }}: <b>{{ state.status.qos?.streamCount || 0 }}</b></span>
          <span>{{ t('qos.throttled') }}: <b>{{ state.status.qos?.throttled || 0 }}</b></span>
        </div>
        <el-table :data="state.status.qos?.streams || []" size="small" class="mt-2" :empty-text="t('qos.none')">
          <el-table-column prop="id" :label="t('qos.channel')" width="90" />
          <el-table-column prop="role" :label="t('common.name')" width="110" />
          <el-table-column :label="t('qos.kind')" width="120">
            <template #default="{ row }">
              <el-tag :type="row.kind === 'interactive' ? 'success' : 'warning'" size="small">{{ row.kind }}</el-tag>
            </template>
          </el-table-column>
          <el-table-column :label="t('qos.limit')" width="140">
            <template #default="{ row }">{{ row.limitKbps > 0 ? row.limitKbps + ' Kbps' : '不限' }}</template>
          </el-table-column>
          <el-table-column :label="t('qos.avgPkt')" width="120">
            <template #default="{ row }">{{ Math.round(row.avgPktBytes) }} B</template>
          </el-table-column>
          <el-table-column :label="t('qos.sent')" min-width="120">
            <template #default="{ row }">{{ (row.bytes / 1048576).toFixed(2) }} MB</template>
          </el-table-column>
        </el-table>
      </div>
    </div>

    <div class="bt-card p-5">
      <div class="flex items-start justify-between gap-3">
        <div>
          <div class="font-medium" style="color: var(--bt-text)">{{ t('config.transfer') }}</div>
          <div class="text-xs mt-1" style="color: var(--bt-text-secondary)">{{ t('config.transferHint') }}</div>
        </div>
        <div class="flex gap-2 flex-shrink-0">
          <el-button plain :icon="Download" @click="exportConfig">{{ t('config.export') }}</el-button>
          <el-button plain :icon="Upload" @click="pickImport">{{ t('config.import') }}</el-button>
        </div>
      </div>
      <input ref="importInput" type="file" accept="application/json" class="hidden" @change="doImport" />
    </div>

    <div class="bt-card p-5">
      <div class="flex items-center justify-between">
        <div>
          <div class="font-medium" style="color: var(--bt-text)">{{ t('config.webAuth') }}</div>
          <div class="text-xs mt-1" style="color: var(--bt-text-secondary)">{{ t('config.webAuthHint') }}</div>
        </div>
        <el-button plain @click="openPwdDialog">{{ t('config.changePassword') }}</el-button>
      </div>
    </div>

    <el-dialog v-model="pwdOpen" :title="t('config.changePassword')" width="420px">
      <el-form label-position="top">
        <el-form-item :label="t('login.username')">
          <el-input v-model="pwd.username" autocomplete="username" />
        </el-form-item>
        <el-form-item :label="t('config.newPassword')">
          <el-input v-model="pwd.newPassword" type="password" show-password autocomplete="new-password" :placeholder="t('config.pwdHint')" />
        </el-form-item>
        <el-form-item :label="t('config.confirmPassword')">
          <el-input v-model="pwd.confirm" type="password" show-password autocomplete="new-password" />
        </el-form-item>
        <div class="text-xs" style="color: var(--bt-text-muted)">{{ t('config.noOldPassword') }}</div>
      </el-form>
      <template #footer>
        <el-button @click="pwdOpen = false">{{ t('common.cancel') }}</el-button>
        <el-button type="primary" :loading="changing" @click="changePassword">{{ t('common.confirm') }}</el-button>
      </template>
    </el-dialog>
  </div>
</template>

<script setup>
import { computed, reactive, ref, watch } from 'vue'
import { ElMessage } from 'element-plus'
import { Download, Upload } from '@element-plus/icons-vue'
import { Delete, Plus } from '@element-plus/icons-vue'
import { api } from '../api'

const MASK = '********'
import { t } from '../i18n'
import { useStore } from '../store'

const store = useStore()
const state = store.state
const saving = ref(false)
const changing = ref(false)
const pwdOpen = ref(false)
const pwd = reactive({ username: state.config.settings?.web_user || 'admin', newPassword: '', confirm: '' })

function openPwdDialog() {
  pwd.username = state.config.settings?.web_user || 'admin'
  pwd.newPassword = ''
  pwd.confirm = ''
  pwdOpen.value = true
}

const qosSaving = ref(false)
const qos = reactive({
  enabled: false,
  mode: 'auto',
  totalMbps: 0,
  interactivePercent: 20,
  interactiveFloorKbps: 320,
})

// Hydrate from the live scheduler state, which carries the persisted policy.
watch(
  () => state.status.qos,
  (v) => {
    if (!v || !v.config) return
    qos.enabled = !!v.config.enabled
    qos.mode = v.config.mode || 'auto'
    qos.totalMbps = Math.round((v.config.totalKbps || 0) / 1000)
    qos.interactivePercent = v.config.interactivePercent || 20
    qos.interactiveFloorKbps = v.config.interactiveFloorKbps || 320
  },
  { immediate: true, deep: true },
)

async function saveQos() {
  qosSaving.value = true
  try {
    const res = await api.setQos({
      enabled: qos.enabled,
      mode: qos.mode,
      totalKbps: Math.round((qos.totalMbps || 0) * 1000),
      interactivePercent: qos.interactivePercent,
      interactiveFloorKbps: qos.interactiveFloorKbps,
    })
    if (res.ok === false) ElMessage.warning(res.error)
    else ElMessage.success(qos.enabled ? '流量调度已开启' : '流量调度已关闭')
  } catch (e) {
    ElMessage.error(e.message)
  } finally {
    qosSaving.value = false
  }
}

function uid() {
  return Math.random().toString(36).slice(2, 10)
}

function addServer() {
  state.config.servers.push({ id: uid(), alias: '新服务端', bindHost: '0.0.0.0', port: 33891, password: '', commonName: 'mail.qq.com', enabled: true })
  store.markDirty()
}

function removeServer(idx) {
  state.config.servers.splice(idx, 1)
  store.markDirty()
}

function addClient() {
  state.config.clients.push({ id: uid(), alias: '新上游', host: '', port: 33891, password: '', clientId: 'client-1', sni: 'mail.qq.com', enabled: true })
  store.markDirty()
}

function removeClient(idx) {
  state.config.clients.splice(idx, 1)
  store.markDirty()
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

const importInput = ref(null)

// The API only returns a mask for passwords. When the user focuses a password
// field we fetch the real value so they can read or copy it (a client password
// has to match the server's, which is how the two get compared).
async function reveal(row, kind) {
  if (row.password !== MASK || !row.id) return
  try {
    const r = await api.revealSecret(kind, row.id)
    if (r && typeof r.password === 'string') row.password = r.password
  } catch { /* keep the mask on failure */ }
}

function exportConfig() {
  const blob = new Blob([JSON.stringify(state.config, null, 2)], { type: 'application/json' })
  const a = document.createElement('a')
  a.href = URL.createObjectURL(blob)
  a.download = `mytun-config-${new Date().toISOString().slice(0, 10)}.json`
  a.click()
  URL.revokeObjectURL(a.href)
}

function pickImport() {
  importInput.value?.click()
}

async function doImport(ev) {
  const file = ev.target.files?.[0]
  if (!file) return
  try {
    const text = await file.text()
    const cfg = JSON.parse(text)
    if (!cfg || typeof cfg !== 'object') throw new Error('不是有效的配置文件')
    const res = await api.saveConfig({ ...cfg, revision: state.config.revision })
    if (res?.config) applyImported(res.config)
    if (res?.warning) ElMessage.warning(res.warning)
    else ElMessage.success(t('config.imported'))
  } catch (e) {
    ElMessage.error(e.message)
  } finally {
    ev.target.value = ''
  }
}

function applyImported(cfg) {
  state.config.servers = cfg.servers || []
  state.config.clients = cfg.clients || []
  state.config.forwards = cfg.forwards || []
  state.config.proxies = cfg.proxies || []
  state.config.settings = cfg.settings || {}
  state.config.revision = cfg.revision
  state.dirty = false
}

async function changePassword() {
  if (pwd.newPassword && pwd.newPassword.length < 4) {
    ElMessage.error(t('config.pwdTooShort'))
    return
  }
  if (pwd.newPassword !== pwd.confirm) {
    ElMessage.error(t('config.pwdMismatch'))
    return
  }
  changing.value = true
  try {
    // The session is already authenticated, so no old password is required.
    await api.changePassword({ username: pwd.username, newPassword: pwd.newPassword })
    if (state.config.settings) state.config.settings.web_user = pwd.username
    ElMessage.success(t('config.pwdUpdated'))
    pwdOpen.value = false
  } catch (e) {
    ElMessage.error(e.message)
  } finally {
    changing.value = false
  }
}
</script>
