import { createRouter, createWebHashHistory } from 'vue-router'
import Overview from '../views/Overview.vue'
import Config from '../views/TunnelConfig.vue'
import Tunnels from '../views/Tunnels.vue'
import Proxies from '../views/Proxies.vue'
import Tester from '../views/Tester.vue'
import Logs from '../views/Logs.vue'
import Chat from '../views/Chat.vue'

const routes = [
  { path: '/', redirect: '/overview' },
  { path: '/overview', name: 'Overview', component: Overview },
  { path: '/config', name: 'Config', component: Config },
  { path: '/tunnels', name: 'Tunnels', component: Tunnels },
  { path: '/proxies', name: 'Proxies', component: Proxies },
  { path: '/tester', name: 'Tester', component: Tester },
  { path: '/logs', name: 'Logs', component: Logs },
  { path: '/chat', name: 'Chat', component: Chat },
]

export default createRouter({
  history: createWebHashHistory(),
  routes,
})
