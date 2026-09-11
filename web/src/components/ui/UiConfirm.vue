<script setup lang="ts">
/**
 * 危险操作的二次确认。
 *
 * 替掉原先散在各处的 `window.confirm`。原生弹窗有两个实际问题：
 *  ① 写不下影响范围。停用一个账号会连带影响它下面所有 Key，
 *    "确定？"这三个字给不了这个信息，而那正是决定要不要点的依据。
 *  ② 它是浏览器 UI，puppeteer 默认自动 dismiss —— 验收脚本里那条
 *    "删除 Key"其实一直靠 page.on('dialog') 兜着。
 *
 * `impact` 就是给 ① 用的：调用方把"将影响 12 把 Key"这类话传进来。
 */
defineProps<{
  open: boolean
  title: string
  /** 影响范围。算不出来时传空，**不要编一个** —— 写错的影响面比不写更危险。 */
  impact?: string
  confirmText?: string
  busy?: boolean
}>()
const emit = defineEmits<{ confirm: []; cancel: [] }>()
</script>

<template>
  <div v-if="open" class="drawer-scrim center" @click.self="emit('cancel')">
    <div class="confirm" role="alertdialog" aria-modal="true" :aria-label="title">
      <h2 class="confirm-t">{{ title }}</h2>
      <p v-if="impact !== undefined && impact !== ''" class="confirm-i">{{ impact }}</p>
      <div class="confirm-b"><slot /></div>
      <div class="confirm-a">
        <button class="btn outline sm" :disabled="busy === true" @click="emit('cancel')">
          取消
        </button>
        <button
          class="btn danger sm"
          data-confirm-ok
          :disabled="busy === true"
          @click="emit('confirm')"
        >
          {{ confirmText ?? '确认' }}
        </button>
      </div>
    </div>
  </div>
</template>
