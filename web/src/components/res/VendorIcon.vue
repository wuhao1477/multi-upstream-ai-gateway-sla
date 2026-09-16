<script setup lang="ts">
/**
 * 发行方图标。有 SVG 用 SVG，没有退回名字取色的字母块。
 *
 * 退回而不是留空：站点随时会加我们没收图标的 vendor，"缺图标"不该变成
 * "这一格空着"——空格子读起来像"没有发行方"，而那是另一回事（未声明）。
 */
import { computed } from 'vue'
import { vendorHue, vendorIconURL, vendorInitial } from '@/utils/vendorIcon'

const props = defineProps<{ name: string; icon?: string | null }>()

const url = computed(() => vendorIconURL(props.icon))
</script>

<template>
  <img
    v-if="url !== null"
    class="vmark img"
    :src="url"
    :alt="name"
    :data-vendor-icon="icon ?? ''"
    loading="lazy"
  />
  <span
    v-else
    class="vmark"
    :style="{ '--vh': vendorHue(name) }"
    :data-vendor-icon="''"
    :title="name"
    >{{ vendorInitial(name) }}</span
  >
</template>
