<template>
  <div class="min-h-screen flex items-center justify-center px-4" :style="{ background: 'var(--bt-bg)' }">
    <div class="w-full max-w-md">
      <div class="flex flex-col items-center gap-3 mb-8">
        <div class="w-14 h-14 rounded-2xl flex items-center justify-center" style="background: var(--bt-gradient); box-shadow: var(--bt-card-shadow-hover)">
          <el-icon :size="28" color="#fff"><Connection /></el-icon>
        </div>
        <div class="text-center">
          <h1 class="text-2xl font-semibold" style="color: var(--bt-text)">{{ t('login.title') }}</h1>
          <p class="text-sm mt-1" style="color: var(--bt-text-secondary)">{{ t('login.subtitle') }}</p>
        </div>
      </div>

      <div class="bt-card p-6">
        <el-form :model="form" label-position="top" @submit.prevent="submit">
          <el-form-item :label="t('login.username')">
            <el-input v-model="form.username" size="large" autocomplete="username" :prefix-icon="User" />
          </el-form-item>
          <el-form-item :label="t('login.password')">
            <el-input
              v-model="form.password"
              type="password"
              size="large"
              show-password
              autocomplete="current-password"
              :prefix-icon="Lock"
              @keyup.enter="submit"
            />
          </el-form-item>
          <el-alert v-if="error" :title="error" type="error" :closable="false" show-icon class="mb-3" />
          <el-button type="primary" size="large" class="w-full" :loading="loading" @click="submit">
            {{ t('login.submit') }}
          </el-button>
        </el-form>
      </div>

      <div class="flex items-center justify-center gap-4 mt-6">
        <div class="bt-theme-toggle" :title="isDark ? t('login.themeDarkTitle') : t('login.themeLightTitle')" @click="toggleTheme">
          {{ isDark ? '☀️' : '🌙' }}
        </div>
        <div class="bt-locale-toggle" @click="toggleLocale">{{ locale === 'zh' ? 'EN' : 'ZH' }}</div>
      </div>
    </div>
  </div>
</template>

<script setup>
import { reactive, ref } from 'vue'
import { User, Lock } from '@element-plus/icons-vue'
import { api } from '../api'
import { t, locale, toggleLocale } from '../i18n'

const props = defineProps({ isDark: { type: Boolean, default: true } })
const emit = defineEmits(['login-success', 'toggle-theme'])

const form = reactive({ username: 'admin', password: '' })
const loading = ref(false)
const error = ref('')

async function submit() {
  if (loading.value) return
  loading.value = true
  error.value = ''
  try {
    await api.login(form.username, form.password)
    emit('login-success')
  } catch (e) {
    error.value = e.status === 401 ? t('login.error') : e.message
  } finally {
    loading.value = false
  }
}

function toggleTheme() {
  emit('toggle-theme')
}
</script>
