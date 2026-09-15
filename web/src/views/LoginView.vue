<script setup lang="ts">
/**
 * 登录页。**只要一个 ADMIN_TOKEN**——P1 没有平台用户实体（`gateway_clients`
 * 已在 022 迁移里 DROP），所以这里没有用户名，也不该有。
 *
 * 令牌只存 localStorage，绝不落服务端（06 §1 的边界）。
 *
 * 提交时拿一个**真实的鉴权端点**验一次再放行，而不是"存下就跳转"。
 * 后者的症状是：跳进控制台，每一栏各弹一个失败 toast，然后被 401 弹回登录页 ——
 * 三步之后人才知道令牌打错了，而且中间那一屏看起来像是系统坏了。
 */
import { ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import ThemeSwitch from '@/components/layout/ThemeSwitch.vue'
import { listChannels } from '@/api/admin'
import { ApiError } from '@/api/client'
import { useAuthStore } from '@/stores/auth'

const route = useRoute()
const router = useRouter()
const auth = useAuthStore()

const input = ref('')
const err = ref('')
const busy = ref(false)

async function submit(): Promise<void> {
  const t = input.value.trim()
  if (t === '') {
    err.value = '请填入 ADMIN_TOKEN'
    return
  }
  busy.value = true
  err.value = ''
  // 先落盘：api() 读的是 localStorage，不是这个组件里的变量。
  // 必须是同步的 —— 下一行就拿它发请求（见 stores/auth.ts 的注释）。
  auth.set(t)
  try {
    await listChannels()
  } catch (e) {
    auth.set('')
    err.value =
      e instanceof ApiError && e.status === 401
        ? '令牌不对。它是后端启动时的 ADMIN_TOKEN 环境变量，不是上游站点的 Key。'
        : `连不上后端：${e instanceof Error ? e.message : String(e)}`
    return
  } finally {
    busy.value = false
  }
  // 回到原本要去的地方。**必须校验它是站内路径**：next 来自 URL，
  // `?next=https://…` 会把人送去外站，而地址栏上一刻还是我们自己的域名。
  const next = typeof route.query.next === 'string' ? route.query.next : ''
  await router.replace(next.startsWith('/') && !next.startsWith('//') ? next : '/')
}
</script>

<template>
  <div class="login" id="pane-login">
    <div class="login-top"><ThemeSwitch /></div>
    <form class="login-box" @submit.prevent="submit">
      <div class="brand">
        <div class="brand-mark">SLA</div>
        <div>
          <div class="brand-t">上游采集与管理</div>
          <div class="brand-s">交付阶段 P1</div>
        </div>
      </div>
      <p class="login-d">
        填入后端的 <code>ADMIN_TOKEN</code> 即可进入。令牌只存在这台浏览器里，
        <b>不会上传</b>，下次打开不用再填。
      </p>
      <label for="token">管理令牌 ADMIN_TOKEN</label>
      <!-- type=password + v-model：明文只在 DOM 属性里（property，不是序列化
           attribute），page.content() 抓不到，符合 FR-094 的"不回显"。 -->
      <input
        id="token"
        v-model="input"
        type="password"
        placeholder="粘贴令牌"
        autocomplete="off"
        autofocus
      />
      <p class="login-e" id="login-error" v-if="err !== ''">{{ err }}</p>
      <button class="btn" id="login-submit" type="submit" :disabled="busy">
        {{ busy ? '校验中…' : '进入' }}
      </button>
    </form>
  </div>
</template>
