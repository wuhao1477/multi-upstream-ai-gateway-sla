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
 * 两种视图，**同一份数据、同一套筛选、同一个抽屉**：
 *  · 列表 —— 一屏看得下几十行，扫"哪些模型到处都有"用它。默认。
 *  · 卡片 —— 一眼看到名字、口径、价格区间（NewAPI 那种形态）。
 * 切视图不重拉数据也不重置筛选：两边渲染的是 store 里同一个 items。
 *
 * 明细一律走抽屉，列表模式**没有行内展开**（2026-09-14 去掉）。原先列表用
 * 行内展开、卡片用抽屉，于是同一份明细有两套容器与两套宽度约束 —— 那张表有
 * 七列，塞进表格行里必须挤掉几列，而挤掉哪几列又与抽屉里不一致。一套就够。
 */
import { computed, onMounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import ModelChannelTable from '@/components/res/ModelChannelTable.vue'
import ScopePicker from '@/components/res/ScopePicker.vue'
import VendorIcon from '@/components/res/VendorIcon.vue'
import UiCard from '@/components/ui/UiCard.vue'
import UiDrawer from '@/components/ui/UiDrawer.vue'
import UiEmpty from '@/components/ui/UiEmpty.vue'
import UiSegmented from '@/components/ui/UiSegmented.vue'
import UiStat from '@/components/ui/UiStat.vue'
import { useChannelsStore } from '@/stores/channels'
import { PAGE_SIZES, useGlobalCatalogStore } from '@/stores/globalCatalog'
import { useResourcesStore } from '@/stores/resources'
import {
  dynamicRateText,
  endpointLabel,
  priceRanges,
  pricingTypeLabel,
  unitChip,
  unitLabel,
} from '@/utils/format'
import type { ModelEntry } from '@/api/types'

type ViewMode = 'list' | 'card'
type Density = 'tight' | 'normal' | 'loose'
const VIEW_KEY = 'sla.models.view'
const DENSITY_KEY = 'sla.models.density'
const PRICE_UNIT_KEY = 'sla.models.priceUnit'

/**
 * 价格显示口径：每 1M token 还是每 1K token。
 *
 * 只影响**显示**，不影响筛选与排序 —— 它是同一个数除以 1000。上游的定价页
 * 也是这么切的，运维两边对照时不用换算。
 */
type PriceUnit = '1M' | '1K'

/** 卡片网格的最小列宽。密度就是这一个数 —— 列数由 auto-fill 自己算。 */
const DENSITY_WIDTH: Record<Density, string> = {
  tight: '190px',
  normal: '280px',
  loose: '380px',
}

const route = useRoute()
const router = useRouter()
const cat = useGlobalCatalogStore()
const res = useResourcesStore()
const channels = useChannelsStore()

/**
 * 「按 Key」分面的候选。不走后端分面：Key 列表本来就在共享 store 里
 * （账号页与 Key 页都要用），为它再发一条分面查询是白发。
 *
 * `usable=false` 的 Key **不可选**：它没有分组，后端按定义匹配不到任何模型，
 * 选中只会得到一个无法解释的空列表。实测 2026-09-14 两个真站点导入的 6 把
 * Key 里有 3 把是这样 —— 上游 /api/token 的 `group` 是空串（站点用
 * `auto_groups` 在调用时自动选组），或者它声明的分组不在我方采到的分组里。
 * 这不是缺陷，是"我们确实不知道它在哪个分组"，所以标出来而不是替它猜。
 */
const keyFacet = computed(() =>
  res.keys.map((k) => ({
    id: k.id,
    label: k.secret_prefix,
    channel: channels.list.find((c) => c.id === k.channel_id)?.name ?? `#${k.channel_id}`,
    group: k.group_ref,
    rate: k.rate_multiplier,
    dynamic: k.rate_dynamic === true,
    rateText: k.rate_dynamic === true
      ? dynamicRateText(k.rate_min, k.rate_max)
      : `×${k.rate_multiplier ?? '?'}`,
    // 「跟账号走」也算可用：那把 Key 自己没定分组，但调用实际走账号的默认分组，
    // 倍率与可调模型都是确定的。标出来只是因为**改法不同**（该改账号那一头）。
    inherited: k.group_inherited === true,
    usable: k.group_ref !== undefined && k.group_ref !== '',
  })),
)

/**
 * 发行方分面先只铺前 VENDOR_HEAD 个，其余收进「展开全部」。
 *
 * 实测一个真站点有 34 家发行方，全铺出来是六七行 chip，把下面的列表挤出首屏 ——
 * 而那几行里绝大多数是只有一两个模型的小厂。按模型数降序取前几个，
 * 剩下的要找时再展开。**已选中的永远显示**，否则选完一收起就看不见自己选了什么。
 */
const VENDOR_HEAD = 12
const vendorsExpanded = ref(false)

const UNGROUPED_HINT =
  '这把 Key 解析不出分组：它自己没定分组，而账号的默认分组也没采到' +
  '（上游没给，或它不在该站的 group_ratio 里）。不知道分组就不知道它能调哪些模型、' +
  '按什么倍率计费 —— 所以筛不了，而不是筛出空。'

/** 输入框自己的值。理由同 CatalogView：直接绑 store.q 会在防抖窗口内被回写。 */
const qInput = ref('')
const view = ref<ViewMode>('list')
const density = ref<Density>('normal')
/** 抽屉里的模型名。空串 = 抽屉关着。 */
const pickedName = ref('')
const priceUnit = ref<PriceUnit>('1M')

/** 把 $/1M 换成当前口径。/1K 就是除以 1000（同一个数，不是另一套价）。 */
function inUnit(usdPer1M: number): number {
  return priceUnit.value === '1K' ? Math.round((usdPer1M / 1000) * 1e6) / 1e6 : usdPer1M
}

/**
 * 主列表那一格价格：**按口径分段**给绝对美元区间。
 *
 * 绝对价而不是裸倍率：倍率跨站点不可比（同一个 ×1 在不同 quota_per_unit 的站上
 * 是不同的钱），折算后才在同一把尺子上。折算不出来（该段没有一个渠道采到基数）
 * 时退回倍率并标出来 —— 不猜一个基数。
 */
function priceText(m: ModelEntry): { unit: string; text: string; usd: boolean }[] {
  return priceRanges(m.channels).map((r) => {
    if (r.usdMin === null || r.usdMax === null) {
      return {
        unit: r.unit,
        text: r.min === r.max ? `${r.min}` : `${r.min} – ${r.max}`,
        usd: false,
      }
    }
    const lo = inUnit(r.usdMin)
    const hi = inUnit(r.usdMax)
    return { unit: r.unit, text: lo === hi ? `$${lo}` : `$${lo} – $${hi}`, usd: true }
  })
}

/**
 * 折算后那个数的后缀。**口径不同后缀不同** —— per_call 折出来的是"每次调用
 * 多少钱"，与 token 数无关，写成 /1M token 是错的。
 */
function usdSuffix(unit: string): string {
  return unit === 'per_call' ? '/次' : `/${priceUnit.value} token`
}

/**
 * 排序选项。**按价格排只在选定了计价口径时可用** —— 两种口径的数值区间重叠
 * （实测按次 0.004~7 vs 倍率 0.01~175），跨口径按价格排会把 $7/次 的视频模型
 * 排在"倍率 175"之前。禁用而不是隐藏：让人看见它存在、以及为什么现在不能用。
 */
const SORTS = [
  { value: '', label: '渠道数' },
  { value: 'name', label: '模型名' },
  { value: 'price', label: '价格' },
] as const
const priceSortHint =
  '按价格排序要先选一种计价类型：按量与按次的数值区间重叠，混在一起排会把 ' +
  '$7/次 的模型排在"倍率 175"之前。'

/**
 * 抽屉盯的是**模型名**而不是那个对象：翻页或改筛选后 store 里的 items 是新对象，
 * 拿着旧对象的抽屉会一直显示上一页的数据，而它看起来完全正常。
 * 列表里没有这个模型了就自动关掉。
 */
const drawerModel = computed<ModelEntry | null>(() =>
  pickedName.value === ''
    ? null
    : (cat.items.find((m) => m.model_name === pickedName.value) ?? null),
)

/** 把当前筛选与视图写回 URL。replace 而非 push：每敲一个字符都进历史栈会废掉返回键。 */
function syncURL(): void {
  const q: Record<string, string> = {}
  if (cat.q !== '') q.q = cat.q
  if (cat.unit !== '') q.unit = cat.unit
  if (cat.channelIDs.length > 0) q.channel = cat.channelIDs.join(',')
  if (cat.vendors.length > 0) q.vendor = cat.vendors.join(',')
  if (cat.endpoints.length > 0) q.endpoint = cat.endpoints.join(',')
  if (cat.keyIDs.length > 0) q.key = cat.keyIDs.join(',')
  if (view.value !== 'list') q.view = view.value
  void router.replace({ query: q })
}

onMounted(async () => {
  // ⚠️ **先把 query 抓下来再 open()**。
  //
  // store 是跨路由存活的：上次离开时若带着筛选，`open()` 把它清空会触发下面
  // 那个 watch → syncURL → `router.replace({query:{}})`，于是刚 push 进来的
  // `?key=3` 在我们读它之前就被自己抹掉了。症状是"从 Key 页点『能调哪些模型』
  // 跳过来，筛选没生效"，而直接刷新同一个地址却好使 —— 那时 store 是干净的，
  // watch 不触发。
  const entry = { ...route.query }
  await cat.open()
  const s = (k: string): string => (typeof entry[k] === 'string' ? String(entry[k]) : '')
  const csv = (k: string): string[] =>
    s(k)
      .split(',')
      .map((x) => x.trim())
      .filter((x) => x !== '')
  // URL 优先于本地偏好，同 KeysView：别人发来的链接必须能覆盖本地记忆
  const urlView = s('view')
  const savedView = localStorage.getItem(VIEW_KEY)
  view.value = (urlView !== '' ? urlView : (savedView ?? 'list')) === 'card' ? 'card' : 'list'
  localStorage.setItem(VIEW_KEY, view.value)
  const savedPriceUnit = localStorage.getItem(PRICE_UNIT_KEY)
  priceUnit.value = savedPriceUnit === '1K' ? '1K' : '1M'
  const savedDensity = localStorage.getItem(DENSITY_KEY)
  density.value =
    savedDensity === 'tight' || savedDensity === 'loose' ? savedDensity : 'normal'

  // 一次性设好全部筛选再拉：逐维设会为每一维各打一次请求，而前面几次的结果
  // 都会被后面的丢弃（store 里那道过期响应的门），纯属白打。
  const f = {
    q: s('q'),
    unit: s('unit'),
    channelIDs: csv('channel')
      .map(Number)
      .filter((v) => Number.isInteger(v) && v > 0),
    vendors: csv('vendor'),
    endpoints: csv('endpoint'),
    keyIDs: csv('key')
      .map(Number)
      .filter((v) => Number.isInteger(v) && v > 0),
  }
  qInput.value = f.q
  if (
    f.q !== '' ||
    f.unit !== '' ||
    f.channelIDs.length > 0 ||
    f.vendors.length > 0 ||
    f.endpoints.length > 0 ||
    f.keyIDs.length > 0
  ) {
    cat.setFilters(f)
  }
  // Key 分面要渠道名与 Key 列表。两个 store 都自带"已拉过就不重拉"。
  void channels.load()
  void res.load()
})

watch(
  [
    () => cat.q,
    () => cat.unit,
    () => cat.channelIDs,
    () => cat.vendors,
    () => cat.endpoints,
    () => cat.keyIDs,
  ],
  syncURL,
)
watch(view, (v) => {
  localStorage.setItem(VIEW_KEY, v)
  syncURL()
})
watch(density, (d) => localStorage.setItem(DENSITY_KEY, d))
watch(priceUnit, (u) => localStorage.setItem(PRICE_UNIT_KEY, u))

/** 切走计价口径时，若排序是「价格」就退回默认 —— 后端也会这么退，两处一致。 */
watch(
  () => cat.unit,
  (u) => {
    if (u === '' && cat.sort === 'price') cat.setSort('')
  },
)

function resetFilters(): void {
  qInput.value = ''
  cat.clearFilters()
}

/** 分面的「全部 X」按钮：只清这一维，别的筛选留着。 */
function resetChannels(): void {
  cat.setFilters({ channelIDs: [] })
}
function resetVendors(): void {
  cat.setFilters({ vendors: [] })
}
function resetEndpoints(): void {
  cat.setFilters({ endpoints: [] })
}
function resetKeys(): void {
  cat.setFilters({ keyIDs: [] })
}

/**
 * ScopePicker 的 v-model 桥。
 *
 * 它是"弹窗里勾完点确定才写回"的语义（见 ScopePicker 顶部），所以这里一次
 * 收到完整选择，直接 setFilters 打**一次**请求 —— 换成逐个 toggle 会为每个
 * 勾选各打一次，而前面几次的结果都会被过期响应那道门丢掉。
 */
const channelPick = computed({
  get: () => cat.channelIDs,
  set: (v: number[]) => cat.setFilters({ channelIDs: v }),
})
const keyPick = computed({
  get: () => cat.keyIDs,
  set: (v: number[]) => cat.setFilters({ keyIDs: v }),
})

/** 折叠后仍要显示的发行方：前 N 个 + 全部已选中的。 */
const vendorShown = computed(() => {
  if (vendorsExpanded.value) return cat.vendorFacet
  const head = cat.vendorFacet.slice(0, VENDOR_HEAD)
  const picked = cat.vendorFacet.filter(
    ([v]) => cat.vendors.includes(v) && !head.some(([h]) => h === v),
  )
  return [...head, ...picked]
})

/**
 * 一个模型在各渠道上的发行方/端点类型的并集。
 *
 * 为什么取并集而不是随便拿第一个渠道的：同一个模型在 A 站可能标了发行方、
 * B 站没标；端点类型更是逐站不同（B 站没接 gemini 端点）。取第一个会让
 * "有没有"取决于渠道排序，而那个顺序与这个问题毫无关系。
 */
function vendorsOf(m: ModelEntry): string[] {
  return [...new Set(m.channels.map((c) => c.vendor_name).filter((v): v is string => !!v))].sort()
}

function endpointsOf(m: ModelEntry): string[] {
  return [...new Set(m.channels.flatMap((c) => c.endpoint_types ?? []))].sort()
}
</script>

<template>
  <div class="pane on mpage" id="pane-models">
    <!-- 页头。标题 + 一句话 + 居中搜索，照上游定价页的形状：进来第一件事多半是
         "找某个模型"，搜索框该在视线正中。

         **不套 UiCard**：这是页面抬头，不是页面里的一块内容。套了卡片之后它
         与下面两栏长得一样重，而它只是一个标题和一个搜索框。

         那几个统计大方块也去掉了：它们数的就是左栏「定价类型」那几个 chip
         上已经印着的数（全部 38 / 按量计费 38），同一个数在一屏里出现两次，
         代价是四百像素的纵向空白。

         ⚠️ #model-q 必须留在结果区**之外**：它一旦落在随响应整块换掉的子树里，
         重渲染会替换输入框节点，焦点与光标随之丢失 —— 症状是"筛选框只认一个
         字符"。这坑渠道目录踩过一次，见 CatalogView 同处注释。放在页头里天然
         满足这条，左栏与右栏怎么重渲染都碰不到它。 -->
    <div class="mhero">
      <h1 class="mhero-t">模型目录</h1>
      <p class="mhero-d">
        上游声明有哪些模型，以及每个模型<b>哪些渠道有</b>。选型看这里，建站看渠道详情。
      </p>
      <label class="sr" for="model-q">按模型名筛选</label>
      <input
        id="model-q"
        v-model="qInput"
        class="mhero-q"
        placeholder="搜索模型，如 claude / gpt-4o / qwen"
        @input="cat.setQuery(qInput)"
      />
    </div>

    <!-- 左筛选 / 右结果。窄屏叠放（见 app.css 的断点）：左栏六组筛选挤到 300px
         以下就开始换行，而那时右边的表格已经在横向滚动了。 -->
    <div class="mlayout">
      <UiCard class="mside">
        <template #header>
          <h2 class="card-t">筛选</h2>
          <div class="spacer"></div>
          <button
            class="btn outline sm"
            id="model-reset"
            :disabled="!cat.filtering"
            @click="resetFilters"
          >
            重置
          </button>
        </template>

        <!-- 每组的计数**在除自己以外的全部筛选之下**统计（后端算的）——
             所以筛了渠道之后发行方只剩这些渠道有的（正是要的），而渠道那一组
             仍然列全，换得了渠道。 -->
        <div class="mfilters" v-if="cat.loaded">
          <!-- 渠道与 Key 走**弹窗**而不是一排 chip：真库 64 个渠道、Key 更多，
               平铺出来把列表挤出首屏；而弹窗里能搜索、能看域名与分组倍率
               （选错渠道的后果是把筛选打到别人家站点上，域名才是身份）。 -->
          <section class="mfilter" v-if="cat.channelFacet.length > 0">
            <div class="mfilter-h">
              <span>上游渠道</span>
              <button
                v-if="cat.channelIDs.length > 0"
                class="linkish sm"
                data-facet-channel=""
                @click="resetChannels"
              >
                清除
              </button>
            </div>
            <ScopePicker
              id="model-f-channel"
              kind="channel"
              multi
              v-model="channelPick"
              placeholder="全部渠道"
            />
            <p class="mfilter-n">{{ cat.channelFacet.length }} 个渠道有目录数据</p>
          </section>

          <section class="mfilter" v-if="keyFacet.length > 0">
            <div class="mfilter-h">
              <span>按 Key</span>
              <button
                v-if="cat.keyIDs.length > 0"
                class="linkish sm"
                data-facet-key=""
                @click="resetKeys"
              >
                清除
              </button>
            </div>
            <ScopePicker id="model-f-key" kind="key" multi v-model="keyPick" placeholder="不限 Key" />
            <p class="mfilter-n" :title="UNGROUPED_HINT">
              {{ keyFacet.filter((k) => k.usable).length }} 把可筛 ·
              {{ keyFacet.filter((k) => !k.usable).length }} 把未归组
            </p>
          </section>

          <!-- 发行方是左栏的主角（照上游定价页）：竖着一列、带图标、带计数，
               扫一眼就知道这批渠道里谁家模型多。跟着渠道筛选走：没选渠道时
               列全部，选了就只剩这些渠道有的。 -->
          <section class="mfilter" v-if="cat.vendorFacet.length > 0">
            <div class="mfilter-h">
              <span
                >模型发行方
                <span class="dim" v-if="cat.channelIDs.length > 0">（已按所选渠道收窄）</span></span
              >
            </div>
            <div class="vlist">
              <button
                class="vrow"
                :class="{ on: cat.vendors.length === 0 }"
                data-facet-vendor=""
                @click="cat.vendors.length > 0 && resetVendors()"
              >
                <span class="vmark all">全</span>
                <span class="vrow-n">全部发行方</span>
                <span class="badge">{{ cat.vendorFacet.length }}</span>
              </button>
              <button
                v-for="[v, n] in vendorShown"
                :key="v"
                class="vrow"
                :class="{ on: cat.vendors.includes(v) }"
                :data-facet-vendor="v"
                @click="cat.toggleVendor(v)"
              >
                <VendorIcon :name="v" :icon="cat.vendorIcons[v]" />
                <span class="vrow-n">{{ v }}</span>
                <span class="badge">{{ n }}</span>
              </button>
            </div>
            <!-- 折叠：实测一个真站点 34 家发行方，全列出来左栏要滚好几屏，
                 而其中多数只有一两个模型。已选中的永远显示（见 vendorShown），
                 否则收起之后看不见自己选了什么 -->
            <button
              v-if="cat.vendorFacet.length > VENDOR_HEAD"
              class="btn outline sm block"
              id="model-vendor-more"
              @click="vendorsExpanded = !vendorsExpanded"
            >
              {{ vendorsExpanded ? '收起' : `展开全部 ${cat.vendorFacet.length} 家` }}
            </button>
          </section>

          <section class="mfilter">
            <div class="mfilter-h"><span>定价类型</span></div>
            <div class="chips">
              <button
                class="chipf"
                :class="{ on: cat.unit === '' }"
                :data-munit="''"
                @click="cat.setUnit('')"
              >
                全部 <span class="badge">{{ cat.whole }}</span>
              </button>
              <!-- 这里用「按量计费 / 按次计费」而不是「×倍率 / 每次」：后者说的是
                   "这个数怎么读"，是价格单元格的口径；分面上问的是"怎么计费" -->
              <button
                v-for="[u, n] in cat.segments"
                :key="u"
                class="chipf"
                :class="{ on: cat.unit === u }"
                :data-munit="u"
                @click="cat.setUnit(u)"
              >
                {{ pricingTypeLabel(u) }} <span class="badge">{{ n }}</span>
              </button>
            </div>
          </section>

          <section class="mfilter" v-if="cat.endpointFacet.length > 0">
            <div class="mfilter-h"><span>端点类型</span></div>
            <div class="chips">
              <button
                class="chipf"
                :class="{ on: cat.endpoints.length === 0 }"
                data-facet-endpoint=""
                @click="cat.endpoints.length > 0 && resetEndpoints()"
              >
                全部 <span class="badge">{{ cat.endpointFacet.length }}</span>
              </button>
              <button
                v-for="[e, n] in cat.endpointFacet"
                :key="e"
                class="chipf"
                :class="{ on: cat.endpoints.includes(e) }"
                :data-facet-endpoint="e"
                @click="cat.toggleEndpoint(e)"
              >
                {{ endpointLabel(e) }} <span class="badge">{{ n }}</span>
              </button>
            </div>
          </section>
        </div>
      </UiCard>

      <UiCard class="mmain">
        <template #header>
          <h2 class="card-t">模型列表</h2>
          <span class="badge" id="model-count">{{
            cat.loaded ? `${cat.total} / ${cat.whole}` : '—'
          }}</span>
          <div class="spacer"></div>
          <!-- 计价口径只改**显示**：/1K 就是 /1M 除以 1000，同一个数。
               上游定价页也是这么切的，运维两边对照时不用换算。 -->
          <UiSegmented
            v-model="priceUnit"
            label="价格显示口径"
            :options="[
              { value: '1M', label: '/1M' },
              { value: '1K', label: '/1K' },
            ]"
          />
          <label class="sr" for="model-sort">排序方式</label>
          <select
            id="model-sort"
            :value="cat.sort"
            @change="cat.setSort(($event.target as HTMLSelectElement).value)"
          >
            <option
              v-for="o in SORTS"
              :key="o.value"
              :value="o.value"
              :disabled="o.value === 'price' && cat.unit === ''"
            >
              {{ o.label }}
            </option>
          </select>
          <UiSegmented
            v-if="view === 'card'"
            v-model="density"
            label="卡片疏密"
            :options="[
              { value: 'tight', label: '紧凑' },
              { value: 'normal', label: '标准' },
              { value: 'loose', label: '宽松' },
            ]"
          />
          <UiSegmented
            v-model="view"
            label="模型目录视图模式"
            :options="[
              { value: 'list', label: '列表' },
              { value: 'card', label: '卡片' },
            ]"
          />
        </template>

        <p class="note" v-if="cat.unit === ''" :title="priceSortHint">
          「按价格排序」要先在左栏选一种<b>定价类型</b>：按量与按次的数值区间重叠，
          混在一起排会把 $7/次 的模型排在"倍率 175"之前。
        </p>

        <!-- 卡片模式：名字 / 发行方 / 价格 / 渠道数一眼看全，明细进抽屉。
             疏密由 --mcard-w 一个变量决定，列数交给 auto-fill 自己算。 -->
        <div
          class="mcards"
          v-if="view === 'card' && cat.items.length > 0"
          :style="{ '--mcard-w': DENSITY_WIDTH[density] }"
          :data-density="density"
        >
          <button
            v-for="m in cat.items"
            :key="m.model_name"
            class="mcard"
            :data-model-card="m.model_name"
            @click="pickedName = m.model_name"
          >
            <div class="mcard-h">
              <VendorIcon
                v-for="v in vendorsOf(m)"
                :key="v"
                :name="v"
                :icon="cat.vendorIcons[v]"
              />
              <code class="mcard-n">{{ m.model_name }}</code>
            </div>
            <div class="mcard-p">
              <!-- 价格**按口径分组**给区间，绝不跨口径取 min：两者数值区间重叠，
                   混着取会让 $0.08/次 显示成"最低 0.08"而同行还有倍率 0.5 -->
              <span v-for="r in priceText(m)" :key="r.unit" class="mcard-pr">
                {{ r.text }}
                <span class="dim">{{ r.usd ? usdSuffix(r.unit) : unitLabel(r.unit) ?? '口径未知' }}</span>
              </span>
              <span v-if="priceText(m).length === 0" class="dim">未采到价格</span>
            </div>
            <div class="mcard-b">
              <span class="badge" :data-model-card-count="m.model_name"
                >{{ m.channel_count }} 个渠道</span
              >
              <span v-for="e in endpointsOf(m)" :key="e" class="badge">{{ endpointLabel(e) }}</span>
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
                <th data-col="model">模型</th>
                <th data-col="vendor">发行方</th>
                <th data-col="units">定价类型</th>
                <th data-col="price">价格</th>
                <th data-col="channels" class="n">渠道数</th>
                <th data-col="risk">其中</th>
                <th data-col="endpoints">端点类型</th>
              </tr>
            </thead>
            <tbody>
              <!-- 整行点开抽屉，**没有行内展开**：明细那张表有七列，塞进表格行里
                   必须挤掉几列，而挤掉哪几列又与抽屉里不一致（见文件头注释） -->
              <tr
                v-for="m in cat.items"
                :key="m.model_name"
                :data-model-row="m.model_name"
                :class="{ open: pickedName === m.model_name }"
              >
                <td data-col="model">
                  <button
                    class="linkish strong"
                    :data-model-open="m.model_name"
                    @click="pickedName = m.model_name"
                  >
                    <code>{{ m.model_name }}</code>
                  </button>
                </td>
                <td data-col="vendor">
                  <!-- 名字单独包一层：图标取不到时 VendorIcon 退回的是一个**带字母的**
                       方块，textContent 会读成 "OOpenAI"。验收断言的是 .vname。 -->
                  <span class="vcell" v-for="v in vendorsOf(m)" :key="v">
                    <VendorIcon :name="v" :icon="cat.vendorIcons[v]" />
                    <span class="vname">{{ v }}</span>
                  </span>
                  <span v-if="vendorsOf(m).length === 0" class="dim">未声明</span>
                </td>
                <td data-col="units">
                  <span
                    v-for="u in [...new Set(m.channels.map((c) => c.billing_unit ?? 'unknown'))]"
                    :key="u"
                    class="badge"
                    >{{ unitChip(u) }}</span
                  >
                </td>
                <td data-col="price">
                  <span v-for="r in priceText(m)" :key="r.unit" class="pcell">
                    <b>{{ r.text }}</b>
                    <span class="dim">{{
                      r.usd ? usdSuffix(r.unit) : unitLabel(r.unit) ?? '口径未知'
                    }}</span>
                  </span>
                  <span v-if="priceText(m).length === 0" class="dim">未采到</span>
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
                <td data-col="endpoints">
                  <span v-for="e in endpointsOf(m)" :key="e" class="badge">{{
                    endpointLabel(e)
                  }}</span>
                  <span v-if="endpointsOf(m).length === 0" class="dim">未声明</span>
                </td>
              </tr>
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
          style="gap: 9px; align-items: center; margin-top: 11px; flex-wrap: wrap"
        >
          <button
            class="btn outline sm"
            id="model-prev"
            :disabled="!cat.hasPrev"
            @click="cat.prev()"
          >
            ← 上一页
          </button>
          <button
            class="btn outline sm"
            id="model-next"
            :disabled="!cat.hasNext"
            @click="cat.next()"
          >
            下一页 →
          </button>
          <span class="dim" style="font-size: 12px"
            >第 {{ cat.from }}–{{ cat.to }} / 共 {{ cat.total }}{{
              cat.filtering ? '（当前筛选）' : ''
            }}</span
          >
          <label class="sr" for="model-pagesize">每页行数</label>
          <select
            id="model-pagesize"
            style="margin-left: auto; width: auto"
            :value="String(cat.pageSize)"
            @change="cat.setPageSize(Number(($event.target as HTMLSelectElement).value))"
          >
            <option v-for="n in PAGE_SIZES" :key="n" :value="String(n)">每页 {{ n }}</option>
          </select>
        </div>
      </UiCard>
    </div>

    <!-- 这段小字放在页尾而不是页头：它讲的是"读这张表要注意什么"，
         而进页面第一件事是搜模型。顶在上面只会把列表推出首屏。 -->
    <p class="note mfoot">
      这是<b>目录</b>（上游声明它有），不是「已登记为可路由的模型」—— 两者是两层（02
      §1.3）。「疑似下架」按连续缺席采集轮次判定，不按经过时间推测。
      <br />⚠️ 计价口径不同的模型<b>不可直接比大小</b>：<code>/次</code>是每次调用的绝对
      美元价，<code>×倍率</code>是相对基准价的倍数。左栏「定价类型」就是为此存在的。
      <br />「渠道数」是<b>有这个模型</b>的渠道数，不是"能用它"的渠道数 ——
      已停用的渠道不采集也不承接请求，疑似下架的模型上游可能已经撤了，
      两者都单独计数，要不要算进去由你定。
    </p>

    <!-- 两种视图**同一个抽屉**：明细那张表有七列，塞进列表行里必须挤掉几列，
         而挤掉哪几列又与卡片那边不一致。一套就够（见文件头注释）。 -->
    <UiDrawer
      wide
      :open="drawerModel !== null"
      :title="drawerModel?.model_name ?? ''"
      desc="哪些渠道有这个模型。价格与计价口径同格显示，跨口径不可直接比大小。"
      @close="pickedName = ''"
    >
      <template v-if="drawerModel !== null">
        <div class="stats">
          <UiStat label="渠道数" :value="drawerModel.channel_count" />
          <UiStat label="其中疑似下架" :value="drawerModel.stale_count" />
          <UiStat label="其中渠道已停用" :value="drawerModel.disabled_count" />
        </div>
        <div class="flex" style="gap: 6px; flex-wrap: wrap; margin: 10px 0">
          <span class="vcell" v-for="v in vendorsOf(drawerModel)" :key="v">
            <VendorIcon :name="v" :icon="cat.vendorIcons[v]" />
            <span class="vname">{{ v }}</span>
          </span>
          <span v-for="e in endpointsOf(drawerModel)" :key="e" class="badge">{{
            endpointLabel(e)
          }}</span>
        </div>
        <ModelChannelTable :model-name="drawerModel.model_name" :channels="drawerModel.channels" />
        <p class="note">
          「渠道数」是<b>有这个模型</b>的渠道数，不是"能用它"的渠道数：已停用的渠道
          不采集也不承接请求，疑似下架的模型上游可能已经撤了。两者各自计数，
          要不要算进去由你定。
        </p>
      </template>
    </UiDrawer>
  </div>
</template>
