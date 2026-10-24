<template>
  <div v-if="booting" class="min-h-screen flex flex-col items-center justify-center gap-3" :style="{ background: 'var(--bt-bg)' }">
    <el-icon class="is-loading" :size="36" color="var(--el-color-primary)"><Loading /></el-icon>
    <span style="font-size: 14px; color: var(--bt-text-secondary)">{{ t('common.loading') }}</span>
  </div>

  <div v-else-if="!state.authenticated">
    <Login :is-dark="isDark" @login-success="onLoginSuccess" @toggle-theme="toggleTheme" />
  </div>

  <div v-else class="bt-app-shell min-h-screen" :style="{ background: 'var(--bt-bg)' }">
    <header class="bt-header px-6 py-0 flex items-center justify-between flex-wrap gap-y-2 sticky top-0 z-50">
      <div class="flex items-center gap-4 flex-shrink-0">
        <div class="flex items-center gap-3">
          <div class="w-8 h-8 rounded-lg flex items-center justify-center" style="background: var(--bt-gradient)">
            <el-icon :size="18" color="#fff"><Connection /></el-icon>
          </div>
          <span class="bt-logo">MYTUN</span>
        </div>

        <el-tooltip :content="tunnelChipTip" placement="bottom">
          <div class="bt-xray-chip" :class="tunnelOnline ? 'bt-xray-online' : 'bt-xray-offline'">
            <span class="bt-dot" :class="tunnelOnline ? 'bt-dot-active' : 'bt-dot-inactive'"></span>
            <span>TUNNEL</span>
            <span style="opacity: .7">{{ tunnelOnline ? t('header.tunnelOnline') : t('header.tunnelOffline') }}</span>
          </div>
        </el-tooltip>

      </div>

      <div class="flex justify-center flex-1 min-w-0">
        <el-menu :default-active="$route.path" router mode="horizontal" class="bt-nav-header" :ellipsis="false">
          <el-menu-item index="/overview"><el-icon><Odometer /></el-icon> {{ t('nav.overview') }}</el-menu-item>
          <el-menu-item index="/config"><el-icon><Setting /></el-icon> {{ t('nav.config') }}</el-menu-item>
          <el-menu-item index="/tunnels"><el-icon><Connection /></el-icon> {{ t('nav.tunnels') }}</el-menu-item>
          <el-menu-item index="/proxies"><el-icon><Switch /></el-icon> {{ t('nav.proxies') }}</el-menu-item>
          <el-menu-item index="/tester"><el-icon><DataAnalysis /></el-icon> {{ t('nav.tester') }}</el-menu-item>
          <el-menu-item index="/logs"><el-icon><DataLine /></el-icon> {{ t('nav.logs') }}</el-menu-item>
          <el-menu-item index="/chat"><el-icon><ChatDotRound /></el-icon> {{ t('nav.chat') }}</el-menu-item>
        </el-menu>
      </div>

      <div class="flex items-center justify-end gap-3 flex-shrink-0">
        <div v-if="state.dirty" class="bt-unsaved">
          <el-icon><InfoFilled /></el-icon>
          <span>{{ t('header.unsavedChanges') }}</span>
        </div>
        <div class="bt-theme-toggle" :title="isDark ? t('login.themeDarkTitle') : t('login.themeLightTitle')" @click="toggleTheme">
          {{ isDark ? '☀️' : '🌙' }}
        </div>
        <div class="bt-locale-toggle" :title="locale === 'zh' ? 'Switch to English' : '切换到中文'" @click="toggleLocale">
          {{ locale === 'zh' ? 'EN' : 'ZH' }}
        </div>
        <el-tooltip :content="t('header.logout')" placement="bottom">
          <el-button circle plain :icon="SwitchButton" @click="doLogout" />
        </el-tooltip>
      </div>
    </header>

    <div class="bt-shell-body">
    <main class="bt-main py-6 px-6">
      <router-view />
    </main>
      <SpeedRail v-if="state.authenticated" />
    </div>
  </div>
</template>

<script setup>
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import { SwitchButton } from '@element-plus/icons-vue'
import Login from './components/Login.vue'
import SpeedRail from './components/SpeedRail.vue'
import { api } from './api'
import { t, locale, toggleLocale } from './i18n'
import { useStore } from './store'

const router = useRouter()
const store = useStore()
const state = store.state

const booting = ref(true)
const isDark = ref(localStorage.getItem('mytun-theme') !== 'light')

const tunnelOnline = computed(() => (state.status.inbound || []).length > 0 || (state.status.clients || []).some((c) => c.status === 'connected'))
const tunnelChipTip = computed(() => {
  const inb = (state.status.inbound || []).map((i) => i.clientId).join(', ')
  const out = (state.status.clients || []).filter((c) => c.status === 'connected').map((c) => `${c.host}:${c.port}`).join(', ')
  return [inb && `inbound: ${inb}`, out && `outbound: ${out}`].filter(Boolean).join(' | ') || 'no tunnel peers'
})

function applyTheme() {
  document.documentElement.classList.toggle('dark', isDark.value)
}

function toggleTheme() {
  isDark.value = !isDark.value
  localStorage.setItem('mytun-theme', isDark.value ? 'dark' : 'light')
  applyTheme()
}

async function onLoginSuccess() {
  await store.bootstrap()
  if (state.authenticated) router.push('/overview')
}



async function doLogout() {
  await store.logout()
  router.push('/overview')
}

onMounted(async () => {
  applyTheme()
  try {
    // Silent sign-in: a valid refresh cookie (7 days) or a loopback connection
    // gets the user straight in without a password prompt.
    try { await api.refresh() } catch { /* no session yet */ }
    await store.bootstrap()
    // Restore the page the user was on: pushing /overview unconditionally sent
    // every refresh back to the home screen.
    const cur = router.currentRoute.value
    if (state.authenticated && (cur.path === '/' || cur.path === '' || cur.meta?.public)) {
      await router.replace('/overview')
    }
  } catch {
    state.authenticated = false
  } finally {
    booting.value = false
  }
})

onUnmounted(() => store.stopStream())
</script>
