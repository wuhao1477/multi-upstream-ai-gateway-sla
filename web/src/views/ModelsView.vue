<script setup lang="ts">
/**
 * 模型目录 —— 跨渠道的选型视角。
 *
 * 渠道详情里那个「模型目录」Tab 回答的是"**这个渠道**有什么模型"，是建站视角；
 * 本分栏回答的是"**这个模型**哪些渠道有"。同一张 `channel_model_catalog`，
 * 两个方向，各自独立成页 —— 把后者塞进渠道详情做不到，那里天然只有一个渠道。
 *
 * ⚠️ 这里是**目录**，不是「可路由模型」：目录说的是"上游声明它有"，
 * 与"我方已登记为可路由"是两层（02 §1.3），所以端点叫 /admin/catalog。
 * 页面上那句 note 不是客套，是防止有人拿这张表当"网关支持哪些模型"的清单。
 *
 * 筛选写进 URL：把"谁家有 claude-opus"这条链接发给别人，对方看到同一屏。
 *
 * 两种视图，**同一份数据、同一套筛选**：
 *  · 列表 —— 一屏看得下几十行，扫"哪些模型到处都有"用它。默认。
 *  · 卡片 —— 一眼看到名字、口径、价格区间，点开抽屉看逐渠道明细（NewAPI 那种形态）。
 * 切视图不重拉数据也不重置筛选：两边渲染的是 store 里同一个 items。
 */
import { computed, onMounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import ModelChannelTable from '@/components/res/ModelChannelTable.vue'
import UiCard from '@/components/ui/UiCard.vue'
import UiDrawer from '@/components/ui/UiDrawer.vue'
import UiEmpty from '@/components/ui/UiEmpty.vue'
import UiSegmented from '@/components/ui/UiSegmented.vue'
import UiStat from '@/components/ui/UiStat.vue'
import { useGlobalCatalogStore } from '@/stores/globalCatalog'
import { priceRanges, unitChip, unitLabel } from '@/utils/format'
import type { ModelEntry } from '@/api/types'

type ViewMode = 'list' | 'card'
const VIEW_KEY = 'sla.models.view'

const route = useRoute()
const router = useRouter()
const cat = useGlobalCatalogStore()

/** 输入框自己的值。理由同 CatalogView：直接绑 store.q 会在防抖窗口内被回写。 */
const qInput = ref('')
/** 展开的模型名（列表模式）。允许多个 —— 比较两个模型的渠道分布是常见动作。 */
const expanded = ref<Set<string>>(new Set())
const view = ref<ViewMode>('list')
/** 抽屉里的模型（卡片模式）。null = 抽屉关着。 */
const picked = ref<ModelEntry | null>(null)

function toggle(name: string): void {
  const next = new Set(expanded.value)
  if (next.has(name)) next.delete(name)
  else next.add(name)
  expanded.value = next
}

/**
 * 抽屉盯的是**模型名**而不是那个对象：翻页或改筛选后 store 里的 items 是新对象，
 * 拿着旧对象的抽屉会一直显示上一页的数据，而它看起来完全正常。
 * 列表里没有这个模型了就自动关掉。
 */
const drawerModel = computed(() =>
  picked.value === null
    ? null
    : (cat.items.find((m) => m.model_name === picked.value?.model_name) ?? null),
)

/** 把当前筛选与视图写回 URL。replace 而非 push：每敲一个字符都进历史栈会废掉返回键。 */
function syncURL(): void {
  const q: Record<string, string> = {}
  if (cat.q !== '') q.q = cat.q
  if (cat.unit !== '') q.unit = cat.unit
  if (view.value !== 'list') q.view = view.value
  void router.replace({ query: q })
}

onMounted(async () => {
  await cat.open()
  // URL 优先于 store 的初始值：别人发来的链接必须能覆盖。open() 里重置过筛选，
  // 所以这里只在 URL 真带了参数时再设一次，避免多打一次空请求。
  const s = (k: string): string => (typeof route.query[k] === 'string' ? String(route.query[k]) : '')
  const urlQ = s('q')
  const urlUnit = s('unit')
  const urlView = s('view')
  // URL 里的视图优先于本地偏好，同 KeysView：别人发来的链接必须能覆盖本地记忆
  const saved = localStorage.getItem(VIEW_KEY)
  const v = urlView !== '' ? urlView : (saved ?? 'list')
  view.value = v === 'card' ? 'card' : 'list'
  localStorage.setItem(VIEW_KEY, view.value)
  if (urlUnit !== '') cat.setUnit(urlUnit)
  if (urlQ !== '') {
    qInput.value = urlQ
    cat.setQuery(urlQ)
  }
})

watch([() => cat.q, () => cat.unit], syncURL)
watch(view, (v) => {
  localStorage.setItem(VIEW_KEY, v)
  // 换视图先关抽屉：列表模式下它没有入口，留着就成了一个关不掉的浮层
  if (v === 'list') picked.value = null
  syncURL()
})
</script>

<template>
  <div class="pane on" id="pane-models">
    <UiCard>
      <template #header>
        <div>
          <h2 class="card-t">模型目录</h2>
          <p class="card-d">
            上游声明有哪些模型，以及每个模型<b>哪些渠道有</b>。选型看这里，建站看渠道详情。
          </p>
        </div>
      </template>

      <!-- 叫「匹配模型数」而不是「模型数」：它跟着筛选框走（分段不影响它）。
           搜 gpt-4o 时这一格是 28，写成「模型数」会被读成"目录里一共 28 个"。 -->
      <div class="stats">
        <UiStat label="匹配模型数" :value="cat.loaded ? cat.whole : '—'" />
        <UiStat
          v-for="[u, n] in cat.segments"
          :key="u"
          :label="unitChip(u)"
          :value="n"
        />
      </div>
      <p class="note">
        这是<b>目录</b>（上游声明它有），不是「已登记为可路由的模型」—— 两者是两层（02
        §1.3）。「疑似下架」按连续缺席采集轮次判定，不按经过时间推测。
        <br />⚠️ 计价口径不同的模型<b>不可直接比大小</b>：<code>/次</code>是每次调用的绝对
        美元价，<code>×倍率</code>是相对基准价的倍数。分段按钮就是为此存在的。
        <br />「渠道数」是<b>有这个模型</b>的渠道数，不是"能用它"的渠道数 ——
        已停用的渠道不采集也不承接请求，疑似下架的模型上游可能已经撤了，
        两者都单独计数，要不要算进去由你定。
      </p>
    </UiCard>

    <UiCard>
      <template #header>
        <h2 class="card-t">模型列表</h2>
        <span class="badge" id="model-count">{{
          cat.loaded ? `${cat.total} / ${cat.whole}` : '—'
        }}</span>
        <div class="spacer"></div>
        <UiSegmented
          v-model="view"
          label="模型目录视图模式"
          :options="[
            { value: 'list', label: '列表' },
            { value: 'card', label: '卡片' },
          ]"
        />
      </template>

      <!-- ⚠️ #model-q 必须留在结果区**之外**：它一旦落在随响应整块换掉的子树里，
           重渲染会替换输入框节点，焦点与光标随之丢失 —— 症状是"筛选框只认一个
           字符"。这坑渠道目录踩过一次，见 CatalogView 同处注释。 -->
      <div class="flex" style="gap: 7px; align-items: center; margin-bottom: 11px">
        <button
          class="btn sm"
          :class="cat.unit === '' ? '' : 'outline'"
          :data-munit="''"
          @click="cat.setUnit('')"
        >
          全部 <span class="badge">{{ cat.whole }}</span>
        </button>
        <button
          v-for="[u, n] in cat.segments"
          :key="u"
          class="btn sm"
          :class="cat.unit === u ? '' : 'outline'"
          :data-munit="u"
          @click="cat.setUnit(u)"
        >
          {{ unitChip(u) }} <span class="badge">{{ n }}</span>
        </button>
        <label class="sr" for="model-q">按模型名筛选</label>
        <input
          id="model-q"
          v-model="qInput"
          placeholder="按模型名筛选，如 claude / gpt-4o"
          style="width: 240px; margin-left: auto"
          @input="cat.setQuery(qInput)"
        />
      </div>

      <!-- 卡片模式：名字 / 口径 / 价格区间 / 渠道数一眼看全，明细进抽屉。 -->
      <div class="mcards" v-if="view === 'card' && cat.items.length > 0">
        <button
          v-for="m in cat.items"
          :key="m.model_name"
          class="mcard"
          :data-model-card="m.model_name"
          @click="picked = m"
        >
          <code class="mcard-n">{{ m.model_name }}</code>
          <div class="mcard-p">
            <!-- 价格**按口径分组**给区间，绝不跨口径取 min：两者数值区间重叠，
                 混着取会让 $0.08/次 显示成"最低 0.08"而同行还有倍率 0.5 -->
            <span v-for="r in priceRanges(m.channels)" :key="r.unit" class="mcard-pr">
              {{ r.min === r.max ? r.min : `${r.min} – ${r.max}` }}
              <span class="dim">{{ unitLabel(r.unit) ?? '口径未知' }}</span>
            </span>
            <span v-if="priceRanges(m.channels).length === 0" class="dim">未采到价格</span>
          </div>
          <div class="mcard-b">
            <span class="badge" :data-model-card-count="m.model_name"
              >{{ m.channel_count }} 个渠道</span
            >
            <span v-if="m.stale_count > 0" class="badge warn">{{ m.stale_count }} 疑似下架</span>
            <span v-if="m.disabled_count > 0" class="badge bad"
              >{{ m.disabled_count }} 已停用</span
            >
          </div>
        </button>
      </div>

      <div class="tw" v-else-if="view === 'list' && cat.items.length > 0">
        <table>
          <thead>
            <tr>
              <th class="x"></th>
              <th data-col="model">模型</th>
              <th data-col="channels" class="n">渠道数</th>
              <th data-col="risk">其中</th>
              <th data-col="units">计价口径</th>
            </tr>
          </thead>
          <tbody>
            <template v-for="m in cat.items" :key="m.model_name">
              <tr :data-model-row="m.model_name" :class="{ open: expanded.has(m.model_name) }">
                <td class="x">
                  <button
                    class="twist"
                    :data-model-toggle="m.model_name"
                    :aria-expanded="expanded.has(m.model_name)"
                    :aria-label="`展开 ${m.model_name} 的渠道`"
                    @click="toggle(m.model_name)"
                  >
                    {{ expanded.has(m.model_name) ? '▾' : '▸' }}
                  </button>
                </td>
                <td data-col="model">
                  <code>{{ m.model_name }}</code>
                </td>
                <td data-col="channels" class="n">
                  <b :data-model-chcount="m.model_name">{{ m.channel_count }}</b>
                </td>
                <td data-col="risk">
                  <!-- 两个计数**各自显示、互不相减**：一个渠道既可能停用又可能
                       陈旧，合成一个"可用数"需要先定义可用，而 P1 没有那个定义 -->
                  <span v-if="m.stale_count > 0" class="badge warn"
                    >{{ m.stale_count }} 个疑似下架</span
                  >
                  <span v-if="m.disabled_count > 0" class="badge bad"
                    >{{ m.disabled_count }} 个渠道已停用</span
                  >
                  <span v-if="m.stale_count === 0 && m.disabled_count === 0" class="dim">—</span>
                </td>
                <td data-col="units">
                  <span
                    v-for="u in [...new Set(m.channels.map((c) => c.billing_unit ?? 'unknown'))]"
                    :key="u"
                    class="badge"
                    >{{ unitChip(u) }}</span
                  >
                </td>
              </tr>

              <!-- 二级展开：哪些渠道有这个模型。数据随列表一起返回，展开不发请求 -->
              <tr v-if="expanded.has(m.model_name)" class="sub-row">
                <td colspan="5">
                  <div class="expand">
                    <div class="sec-t">
                      支持该模型的渠道
                      <span class="badge">{{ m.channel_count }} 个</span>
                    </div>
                    <ModelChannelTable :model-name="m.model_name" :channels="m.channels" />
                  </div>
                </td>
              </tr>
            </template>
          </tbody>
        </table>
      </div>
      <UiEmpty v-else-if="cat.loaded && cat.filtering">当前筛选没有匹配的模型</UiEmpty>
      <UiEmpty v-else-if="cat.loaded"
        >目录为空。先在「渠道管理」里建渠道、登记采集凭证，再点「立即采集」</UiEmpty
      >

      <div
        class="flex"
        v-if="cat.items.length > 0"
        style="gap: 9px; align-items: center; margin-top: 11px"
      >
        <button class="btn outline sm" id="model-prev" :disabled="!cat.hasPrev" @click="cat.prev()">
          ← 上一页
        </button>
        <button class="btn outline sm" id="model-next" :disabled="!cat.hasNext" @click="cat.next()">
          下一页 →
        </button>
        <span class="dim" style="font-size: 12px"
          >第 {{ cat.from }}–{{ cat.to }} / 共 {{ cat.total }}{{
            cat.filtering ? '（当前筛选）' : ''
          }}</span
        >
      </div>
      <p class="note">
        默认按「支持它的渠道数」降序 —— 这一屏先回答"哪些模型到处都有"。
        找具体某个模型用上面的筛选框，不用翻页。
      </p>
    </UiCard>

    <!-- 卡片模式的二级抽屉。列表模式用行内展开，不用它：那边一行就在眼前，
         为看六列明细盖住半屏是倒退。 -->
    <UiDrawer
      :open="drawerModel !== null"
      :title="drawerModel?.model_name ?? ''"
      desc="哪些渠道有这个模型。价格与计价口径同格显示，跨口径不可直接比大小。"
      @close="picked = null"
    >
      <template v-if="drawerModel !== null">
        <div class="stats">
          <UiStat label="渠道数" :value="drawerModel.channel_count" />
          <UiStat label="其中疑似下架" :value="drawerModel.stale_count" />
          <UiStat label="其中渠道已停用" :value="drawerModel.disabled_count" />
        </div>
        <ModelChannelTable
          :model-name="drawerModel.model_name"
          :channels="drawerModel.channels"
        />
        <p class="note">
          「渠道数」是<b>有这个模型</b>的渠道数，不是"能用它"的渠道数：已停用的渠道
          不采集也不承接请求，疑似下架的模型上游可能已经撤了。两者各自计数，
          要不要算进去由你定。
        </p>
      </template>
    </UiDrawer>
  </div>
</template>
