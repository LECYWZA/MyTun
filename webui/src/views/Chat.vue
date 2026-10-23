<template>
  <div class="flex flex-col gap-5">
    <div class="flex items-start justify-between flex-wrap gap-3">
      <div>
        <h2 class="text-xl font-semibold" style="color: var(--bt-text)">{{ t('chat.title') }}</h2>
        <p class="text-sm mt-1" style="color: var(--bt-text-secondary)">{{ t('chat.subtitle') }}</p>
      </div>
      <el-tag :type="online ? 'success' : 'danger'" size="small">{{ online ? 'online' : t('chat.noTunnel') }}</el-tag>
    </div>

    <div class="grid grid-cols-1 xl:grid-cols-3 gap-4">
      <div class="bt-card p-4 xl:col-span-2 flex flex-col" style="min-height: 520px">
        <div class="flex-1 overflow-y-auto flex flex-col gap-2 mb-3" style="max-height: 520px">
          <div v-for="m in messages" :key="m.id" class="flex" :class="m.dir === 'out' ? 'justify-end' : 'justify-start'">
            <div class="rounded-xl px-3 py-2 max-w-[75%]" :style="{
              background: m.dir === 'out' ? 'var(--bt-primary-light)' : 'var(--bt-surface-hover)',
              color: 'var(--bt-text)',
            }">
              <div class="text-xs mb-1" style="color: var(--bt-text-secondary)">
                {{ m.dir === 'out' ? 'me' : m.from }} · {{ new Date(m.ts).toLocaleTimeString() }}
              </div>
              <div class="text-sm whitespace-pre-wrap break-words">{{ m.text }}</div>
            </div>
          </div>
          <el-empty v-if="!messages.length" :description="t('common.none')" :image-size="60" />
        </div>

        <div class="flex gap-2">
          <el-input
            v-model="draft"
            type="textarea"
            :rows="2"
            :placeholder="t('chat.placeholder')"
            :disabled="!online"
            @keydown.enter.exact.prevent="send"
          />
          <div class="flex flex-col gap-2">
            <el-button type="primary" :disabled="!online || !draft.trim()" :loading="sending" @click="send">{{ t('chat.send') }}</el-button>
            <el-button plain :disabled="!online" :loading="uploading" @click="pickFile">{{ t('chat.upload') }}</el-button>
            <input ref="fileInput" type="file" class="hidden" @change="onFile" />
          </div>
        </div>
      </div>

      <div class="bt-card p-4">
        <div class="flex items-center justify-between mb-2">
          <div class="font-medium" style="color: var(--bt-text)">{{ t('chat.files') }}</div>
          <el-button size="small" plain :icon="Refresh" @click="loadFiles" />
        </div>
        <el-table :data="files" size="small" empty-text="—">
          <el-table-column prop="name" :label="t('common.name')" min-width="140" show-overflow-tooltip />
          <el-table-column :label="t('chat.size')" width="90">
            <template #default="{ row }">{{ fmtSize(row.size) }}</template>
          </el-table-column>
          <el-table-column :label="t('common.actions')" width="80">
            <template #default="{ row }">
              <el-button size="small" text type="primary" @click="download(row.name)">{{ t('chat.download') }}</el-button>
            </template>
          </el-table-column>
        </el-table>
      </div>
    </div>
  </div>
</template>

<script setup>
import { computed, onMounted, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { Refresh } from '@element-plus/icons-vue'
import { api } from '../api'
import { t } from '../i18n'
import { useStore } from '../store'

const store = useStore()
const state = store.state
const messages = ref([])
const files = ref([])
const draft = ref('')
const sending = ref(false)
const uploading = ref(false)
const fileInput = ref(null)

const online = computed(() => (state.status.inbound || []).length > 0 || (state.status.clients || []).some((c) => c.status === 'connected'))

function fmtSize(n) {
  const v = Number(n) || 0
  if (v < 1024) return `${v} B`
  if (v < 1048576) return `${(v / 1024).toFixed(1)} KB`
  return `${(v / 1048576).toFixed(2)} MB`
}

async function loadHistory() {
  try {
    const res = await api.chatHistory()
    messages.value = res.messages || []
    files.value = []
  } catch { /* ignore */ }
}

async function loadFiles() {
  try {
    files.value = await api.files()
  } catch (e) {
    ElMessage.error(e.message)
  }
}

async function send() {
  if (!draft.value.trim()) return
  sending.value = true
  try {
    const res = await api.chatSend({ text: draft.value })
    if (!res.ok) throw new Error(res.error || 'send failed')
    draft.value = ''
  } catch (e) {
    ElMessage.error(e.message)
  } finally {
    sending.value = false
  }
}

function pickFile() { fileInput.value?.click() }

async function onFile(ev) {
  const file = ev.target.files?.[0]
  if (!file) return
  uploading.value = true
  try {
    const res = await api.sendFile(file)
    if (!res.ok) throw new Error(res.error || 'upload failed')
    ElMessage.success(t('common.saved'))
    await loadFiles()
  } catch (e) {
    ElMessage.error(e.message)
  } finally {
    uploading.value = false
    ev.target.value = ''
  }
}

function download(name) {
  window.open(`/api/files/${encodeURIComponent(name)}`, '_blank')
}

onMounted(() => {
  loadHistory()
  loadFiles()
})
</script>
