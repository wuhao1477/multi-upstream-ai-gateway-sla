<script setup lang="ts">
/**
 * 右侧抽屉。承载"登记账号 / 登记 Key / 编辑"这类**带上下文的表单**。
 *
 * 为什么是抽屉而不是原来的整页表单：登记 Key 要先知道渠道 ID 和账号 ID，
 * 原来的做法是让人在渠道列表抄一个数字、再回到另一个分栏粘进去。抽屉从
 * 当前那一行打开，上下文由调用方直接填进去，不需要抄。
 *
 * 为什么不是弹窗：这些表单有 4~6 个字段外加说明文字，居中弹窗会盖住
 * 用户刚才在看的那一行，而那一行往往正是他要照着填的依据。
 *
 * 与 UiField 同理，**表单控件的 id 由调用方写**，不藏进组件。
 */
import { onBeforeUnmount, watch } from 'vue'

const props = defineProps<{ open: boolean; title: string; desc?: string }>()
const emit = defineEmits<{ close: [] }>()

/** Esc 关闭。表单里按 Esc 是肌肉记忆，没有它就只能去找那个 × 。 */
function onKey(e: KeyboardEvent): void {
  if (e.key === 'Escape') emit('close')
}

watch(
  () => props.open,
  (open) => {
    if (open) window.addEventListener('keydown', onKey)
    else window.removeEventListener('keydown', onKey)
  },
  { immediate: true },
)

// 抽屉开着时组件被卸载（切分栏），监听器会留下来吃掉后续所有 Esc。
onBeforeUnmount(() => window.removeEventListener('keydown', onKey))
</script>

<template>
  <!-- v-if 而不是 v-show：关闭时表单必须真的从 DOM 上消失。里面有 Key 明文
       输入框，留在 DOM 里等于把它留在页面上（FR-094 的同一条理由）。 -->
  <div v-if="open" class="drawer-scrim" @click.self="emit('close')">
    <aside class="drawer" role="dialog" aria-modal="true" :aria-label="title">
      <header class="drawer-h">
        <div>
          <h2 class="drawer-t">{{ title }}</h2>
          <p v-if="desc !== undefined" class="drawer-d">{{ desc }}</p>
        </div>
        <button class="btn ghost sm drawer-x" aria-label="关闭" @click="emit('close')">✕</button>
      </header>
      <div class="drawer-b"><slot /></div>
      <footer v-if="$slots.footer" class="drawer-f"><slot name="footer" /></footer>
    </aside>
  </div>
</template>
