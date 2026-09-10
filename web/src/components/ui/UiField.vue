<script setup lang="ts">
/**
 * 标签 + 控件。**只管标签与包裹**，控件由调用方自己写 ——
 * 每个输入框的 id 都是验收契约（#ch-name、#cr-token…），
 * 藏进组件里再由 prop 传进去，读代码时就看不出页面上到底有哪些 id 了。
 */
defineProps<{ label: string; for: string }>()
</script>

<template>
  <div>
    <label :for="$props.for">{{ label }}</label>
    <slot />
  </div>
</template>
