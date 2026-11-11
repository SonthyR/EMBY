<script setup lang="ts">
import { onMounted } from 'vue'
import AppShell from '@/layouts/AppShell.vue'
import AuthView from '@/views/AuthView.vue'
import ToastHost from '@/components/ToastHost.vue'
import ConfirmHost from '@/components/ConfirmHost.vue'
import logo from '@/assets/logo.svg'
import { checkSession, session } from '@/stores/session'

onMounted(checkSession)
</script>

<template>
  <div v-if="session.phase === 'checking'" class="splash" aria-busy="true" aria-label="正在加载">
    <img :src="logo" alt="" />
  </div>
  <Transition v-else name="page" mode="out-in" appear>
    <AppShell v-if="session.phase === 'ready'" />
    <AuthView v-else />
  </Transition>
  <ToastHost />
  <ConfirmHost />
</template>

<style scoped>
.splash { display: grid; place-items: center; min-height: 100vh; }
.splash img { width: 44px; height: 44px; opacity: 0.85; animation: breathe 1.4s ease-in-out infinite; }
@keyframes breathe { 50% { opacity: 0.35; transform: scale(0.94); } }
</style>
