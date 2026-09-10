<script setup lang="ts">
/**
 * 分段控件（视图切换 / Tab）。
 *
 * 用 role="tablist" + 方向键而不是一排 button：视图切换是"在几个互斥选项里
 * 选一个"，读屏器要能报出"3 之 1"，键盘用户要能用左右键在段间移动。
 */
defineProps<{
  modelValue: string
  options: { value: string; label: string; count?: number }[]
  /** 无障碍名称，例如"Key 视图模式"。 */
  label: string
}>()
const emit = defineEmits<{ 'update:modelValue': [string] }>()

function move(opts: { value: string }[], cur: string, delta: number): void {
  const i = opts.findIndex((o) => o.value === cur)
  if (i < 0) return
  // 环绕：末段按右键回到首段，比走到头卡住少一次犹豫
  const next = opts[(i + delta + opts.length) % opts.length]
  if (next !== undefined) emit('update:modelValue', next.value)
}
</script>

<template>
  <div class="seg" role="tablist" :aria-label="label">
    <button
      v-for="o in options"
      :key="o.value"
      class="seg-i"
      :class="{ on: modelValue === o.value }"
      role="tab"
      type="button"
      :aria-selected="modelValue === o.value"
      :tabindex="modelValue === o.value ? 0 : -1"
      :data-seg="o.value"
      @click="emit('update:modelValue', o.value)"
      @keydown.left.prevent="move(options, modelValue, -1)"
      @keydown.right.prevent="move(options, modelValue, 1)"
    >
      {{ o.label }}
      <span v-if="o.count !== undefined" class="seg-c">{{ o.count }}</span>
    </button>
  </div>
</template>
