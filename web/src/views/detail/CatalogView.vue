<script setup lang="ts">
import { onMounted, ref } from 'vue'
import UiEmpty from '@/components/ui/UiEmpty.vue'
import UiPrice from '@/components/ui/UiPrice.vue'
import { useCatalogStore } from '@/stores/catalog'
import { useChannelsStore } from '@/stores/channels'
import { unitChip } from '@/utils/format'

const channels = useChannelsStore()
const cat = useCatalogStore()

/** 输入框自己的值。不直接绑 store 的 q：q 只在防抖落地后才变，
 *  绑上去会让输入框在防抖窗口内被回写成旧值（打字看着像掉字符）。 */
const qInput = ref('')
const opened = ref(false)

onMounted(async () => {
  const id = channels.currentID
  if (id === null) return
  await cat.open(id)
  qInput.value = cat.q
  opened.value = true
})
</script>

<template>
  <UiEmpty v-if="opened && cat.whole === 0 && cat.q === ''">目录为空，先点「立即采集」</UiEmpty>
  <template v-else-if="opened">
    <div class="sec-t">模型目录 <span class="badge">共 {{ cat.whole }}</span></div>

    <!-- ⚠️ #cat-q 必须留在**结果区之外**：它一旦落在随请求结果整块换掉的子树里，
         重渲染时输入框节点会被替换，焦点与光标随之丢失 —— 打第二个字就已经
         打到 body 上了，症状是"筛选框只认一个字符"。放在这里 Vue 会原地复用节点，
         不需要手动 focus() + setSelectionRange() 把焦点搬回来。 -->
    <div class="flex" style="gap: 7px; align-items: center; margin-bottom: 11px">
      <!-- 分段按钮读 units（后端在筛选**之前**算的）：切进某段后其余段照旧可见 -->
      <button
        class="btn sm"
        :class="cat.unit === '' ? '' : 'outline'"
        :data-unit="''"
        @click="cat.setUnit('')"
      >
        全部 <span class="badge">{{ cat.whole }}</span>
      </button>
      <button
        v-for="[u, n] in cat.segments"
        :key="u"
        class="btn sm"
        :class="cat.unit === u ? '' : 'outline'"
        :data-unit="u"
        @click="cat.setUnit(u)"
      >
        {{ unitChip(u) }} <span class="badge">{{ n }}</span>
      </button>
      <input
        id="cat-q"
        v-model="qInput"
        placeholder="按模型名筛选"
        style="width: 200px; margin-left: auto"
        @input="cat.setQuery(qInput)"
      />
    </div>

    <div class="tw" v-if="cat.items.length > 0">
      <table>
        <thead>
          <tr>
            <th>模型</th>
            <th>输入价</th>
            <th>输出价</th>
            <th>状态</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="m in cat.items" :key="m.model_name">
            <td>
              <code>{{ m.model_name }}</code>
            </td>
            <td><UiPrice :value="m.input_price" :unit="m.billing_unit" /></td>
            <td><UiPrice :value="m.output_price" :unit="m.billing_unit" /></td>
            <td>
              <span class="badge" :class="m.stale ? 'warn' : ''">{{
                m.stale ? '疑似下架' : '在架'
              }}</span>
            </td>
          </tr>
        </tbody>
      </table>
    </div>
    <UiEmpty v-else>当前筛选没有匹配的模型</UiEmpty>

    <div
      class="flex"
      v-if="cat.items.length > 0"
      style="gap: 9px; align-items: center; margin-top: 11px"
    >
      <button class="btn outline sm" id="cat-prev" :disabled="!cat.hasPrev" @click="cat.prev()">
        ← 上一页
      </button>
      <button class="btn outline sm" id="cat-next" :disabled="!cat.hasNext" @click="cat.next()">
        下一页 →
      </button>
      <span class="dim" style="font-size: 12px"
        >第 {{ cat.from }}–{{ cat.to }} / 共 {{ cat.total }}{{
          cat.filtering ? '（当前筛选）' : ''
        }}</span
      >
    </div>

    <p class="note">
      目录是「上游有什么」，与「已登记为可路由模型」是两层： 目录不需要 token 上界（02 §1.3）。
      「疑似下架」表示模型已连续缺席达到配置轮次，不按经过小时数推测。
      <br />⚠️ 按口径分段排序：<code>/次</code>是每次调用的绝对美元价，
      <code>×倍率</code>是相对基准价的倍数，<b>跨段不可直接比大小</b>。
      分段行数悬殊时（实测单渠道 1161 条倍率 / 208 条按次）用上面的分段按钮切换，
      否则按次那一段要翻二十多页才见得到。
    </p>
  </template>
</template>
