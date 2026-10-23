<template>
  <div class="flex flex-col gap-5">
    <div class="flex items-start justify-between flex-wrap gap-3">
      <div>
        <h2 class="text-xl font-semibold" style="color: var(--bt-text)">{{ t('config.title') }}</h2>
        <p class="text-sm mt-1" style="color: var(--bt-text-secondary)">{{ t('config.subtitle') }}</p>
      </div>
      <el-button type="primary" :loading="saving" @click="save">{{ t('common.save') }}</el-button>
    </div>

    <el-alert type="info" :closable="false" show-icon :title="t('config.hint')" />

    <div class="bt-card p-5">
      <div class="flex items-center justify-between mb-3">
        <div class="font-medium" style="color: var(--bt-text)">{{ t('config.serverSection') }}</div>
        <el-button size="small" plain :icon="Plus" @click="addServer">{{ t('config.addServer') }}</el-button>
      </div>
      <div v-for="(s, idx) in state.config.servers" :key="s.id" class="rounded-xl p-4 mb-3" style="border: 1px solid var(--bt-border)">
        <div class="grid grid-cols-1 md:grid-cols-6 gap-3">
          <el-form-item :label="t('common.name')" class="!mb-0">
            <el-input v-model="s.alias" @input="store.markDirty()" />
          </el-form-item>
          <el-form-item :label="t('config.bindHost')" class="!mb-0">
            <el-input v-model="s.bindHost" @input="store.markDirty()" />
          </el-form-item>
          <el-form-item :label="t('config.tunnelPort')" class="!mb-0">
            <el-input-number v-model="s.port" :min="1" :max="65535" controls-position="right" class="!w-full" @change="store.markDirty()" />
          </el-form-item>
          <el-form-item :label="t('common.password')" class="!mb-0">
            <el-input v-model="s.password" show-password @input="store.markDirty()" />
          </el-form-item>
          <el-form-item :label="t('config.commonName')" class="!mb-0">
            <el-input v-model="s.commonName" @input="store.markDirty()" />
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
        <div class="grid grid-cols-1 md:grid-cols-6 gap-3">
          <el-form-item :label="t('common.name')" class="!mb-0">
            <el-input v-model="c.alias" @input="store.markDirty()" />
          </el-form-item>
          <el-form-item :label="t('config.upstreamHost')" class="!mb-0">
            <el-input v-model="c.host" @input="store.markDirty()" />
          </el-form-item>
          <el-form-item :label="t('config.tunnelPort')" class="!mb-0">
            <el-input-number v-model="c.port" :min="1" :max="65535" controls-position="right" class="!w-full" @change="store.markDirty()" />
          </el-form-item>
          <el-form-item :label="t('common.password')" class="!mb-0">
            <el-input v-model="c.password" show-password @input="store.markDirty()" />
          </el-form-item>
          <el-form-item :label="t('config.clientId')" class="!mb-0">
            <el-input v-model="c.clientId" @input="store.markDirty()" />
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
      <div class="font-medium mb-3" style="color: var(--bt-text)">{{ t('config.webAuth') }}</div>
      <div class="grid grid-cols-1 md:grid-cols-4 gap-3 items-end">
        <el-form-item :label="t('login.username')" class="!mb-0">
          <el-input v-model="pwd.username" />
        </el-form-item>
        <el-form-item :label="t('config.oldPassword')" class="!mb-0">
          <el-input v-model="pwd.oldPassword" type="password" show-password />
        </el-form-item>
        <el-form-item :label="t('config.newPassword')" class="!mb-0">
          <el-input v-model="pwd.newPassword" type="password" show-password />
        </el-form-item>
        <el-button type="primary" plain :loading="changing" @click="changePassword">{{ t('config.changePassword') }}</el-button>
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
const changing = ref(false)
const pwd = reactive({ username: state.config.settings?.web_user || 'admin', oldPassword: '', newPassword: '' })

function uid() {
  return Math.random().toString(36).slice(2, 10)
}

function addServer() {
  state.config.servers.push({ id: uid(), alias: '新服务端', bindHost: '0.0.0.0', port: 33891, password: '', commonName: 'mail.qq.com', enabled: false })
  store.markDirty()
}

function removeServer(idx) {
  state.config.servers.splice(idx, 1)
  store.markDirty()
}

function addClient() {
  state.config.clients.push({ id: uid(), alias: '新上游', host: '', port: 33891, password: '', clientId: 'client-1', sni: 'mail.qq.com', enabled: false })
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

async function changePassword() {
  changing.value = true
  try {
    await api.changePassword(pwd)
    ElMessage.success(t('config.passwordChanged'))
    pwd.oldPassword = ''
    pwd.newPassword = ''
  } catch (e) {
    ElMessage.error(e.message)
  } finally {
    changing.value = false
  }
}
</script>
