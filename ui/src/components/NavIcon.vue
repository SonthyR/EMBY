<script setup lang="ts">
import { LayoutDashboard, Server, Users, ScrollText, Settings, Stethoscope, CircleUser } from '@lucide/vue'
const props = defineProps<{ name?: string }>()
const icons: Record<string, unknown> = {
  dashboard: LayoutDashboard,
  sites: Server,
  share: Users,
  logs: ScrollText,
  settings: Settings,
  diag: Stethoscope,
  account: CircleUser,
}
</script>

<template>
  <component :is="icons[props.name || ''] || Settings" />
</template>
