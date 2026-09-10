<script setup lang="ts">
import { onMounted } from 'vue'
import AppSidebar from '@/components/layout/AppSidebar.vue'
import AppTopbar from '@/components/layout/AppTopbar.vue'
import ToastHost from '@/components/layout/ToastHost.vue'
import { useAuthStore } from '@/stores/auth'
import { useChannelsStore } from '@/stores/channels'
import { useCredentialsStore } from '@/stores/credentials'

const auth = useAuthStore()
const channels = useChannelsStore()
const creds = useCredentialsStore()

// 已经存过令牌就直接拉一轮：刷新后还要再点一次「刷新」是多余的一步。
// 没有令牌则什么都不做 —— 空发请求只会换回 401，把真正该看的提示挤掉。
onMounted(() => {
  if (auth.hasToken) {
    void channels.load()
    void creds.load()
  }
})
</script>

<template>
  <div class="shell">
    <AppSidebar />
    <div class="main">
      <AppTopbar />
      <div class="content">
        <RouterView />
      </div>
    </div>
  </div>
  <ToastHost />
</template>
