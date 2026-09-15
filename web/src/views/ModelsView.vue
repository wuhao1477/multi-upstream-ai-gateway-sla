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
import UiCard from '@/components/ui/UiCard.vue'
import UiDrawer from '@/components/ui/UiDrawer.vue'
import UiEmpty from '@/components/ui/UiEmpty.vue'
import UiSegmented from '@/components/ui/UiSegmented.vue'
import UiStat from '@/components/ui/UiStat.vue'
import { useChannelsStore } from '@/stores/channels'
import { useGlobalCatalogStore } from '@/stores/globalCatalog'
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
 * 供应商图标位。
 *
 * ⚠️ **不是上游那套图标**：NewAPI 用 @lobehub/icons（React 包），而这个界面是
 * Vue 且由 go:embed 打进二进制、要在内网/离线环境可用（见 internal/admin/web.go）
 * —— 走 CDN 会破坏那条保证，引一个 React 图标库到 Vue 工程里也不成立。
 * 这里用**名字首字确定性取色**的字母块：不引依赖、不发外链，仍然让一排 chip
 * 能靠颜色区分开。真图标要么把 lobehub 的静态 SVG 挑需要的几十个 vendor 进
 * 仓库，要么放弃离线保证 —— 那是个取舍，不该由我替你定。
 */
function vendorHue(name: string): number {
  let h = 0
  for (let i = 0; i < name.length; i++) h = (h * 31 + name.charCodeAt(i)) % 360
  return h
}

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

      <!-- 分面筛选。四组都是多选，点第二下取消。
           每组的计数**在除自己以外的全部筛选之下**统计（后端算的）——
           所以筛了渠道之后供应商只剩这些渠道有的（正是要的），而渠道那一组
           仍然列全，换得了渠道。 -->
      <div class="facets" v-if="cat.loaded">
        <div class="facet" v-if="cat.channelFacet.length > 0">
          <div class="facet-l">上游渠道</div>
          <ScopePicker
            id="model-f-channel"
            kind="channel"
            multi
            v-model="channelPick"
            placeholder="全部渠道"
          />
          <button
            v-if="cat.channelIDs.length > 0"
            class="chipf"
            data-facet-channel=""
            @click="resetChannels"
          >
            清除
          </button>
          <span class="dim" style="font-size: 11px"
            >{{ cat.channelFacet.length }} 个渠道有目录数据</span
          >
        </div>

        <!-- 渠道与 Key 走**弹窗**而不是一排 chip：真库 64 个渠道、Key 更多，
             平铺出来把列表挤出首屏；而弹窗里能搜索、能看域名与分组倍率
             （选错渠道的后果是把筛选打到别人家站点上，域名才是身份）。 -->
        <div class="facet" v-if="keyFacet.length > 0">
          <div class="facet-l">按 Key</div>
          <ScopePicker
            id="model-f-key"
            kind="key"
            multi
            v-model="keyPick"
            placeholder="不限 Key"
          />
          <button
            v-if="cat.keyIDs.length > 0"
            class="chipf"
            data-facet-key=""
            @click="resetKeys"
          >
            清除
          </button>
          <span class="dim" style="font-size: 11px" :title="UNGROUPED_HINT"
            >{{ keyFacet.filter((k) => k.usable).length }} 把可筛 ·
            {{ keyFacet.filter((k) => !k.usable).length }} 把未归组</span
          >
        </div>

        <div class="facet" v-if="cat.vendorFacet.length > 0">
          <!-- 供应商跟着渠道筛选走：没选渠道时列全部，选了就只剩这些渠道有的 -->
          <div class="facet-l">
            发行方
            <span class="dim" v-if="cat.channelIDs.length > 0">（已按所选渠道收窄）</span>
          </div>
          <button
            class="chipf"
            :class="{ on: cat.vendors.length === 0 }"
            data-facet-vendor=""
            @click="cat.vendors.length > 0 && resetVendors()"
          >
            全部发行方 <span class="badge">{{ cat.vendorFacet.length }}</span>
          </button>
          <button
            v-for="[v, n] in vendorShown"
            :key="v"
            class="chipf"
            :class="{ on: cat.vendors.includes(v) }"
            :data-facet-vendor="v"
            @click="cat.toggleVendor(v)"
          >
            <span class="vmark" :style="{ '--vh': vendorHue(v) }">{{ [...v][0] }}</span>
            {{ v }} <span class="badge">{{ n }}</span>
          </button>
          <!-- 折叠：实测一个真站点 34 家发行方，全铺是六七行 chip，把列表挤出
               首屏，而其中多数只有一两个模型。已选中的永远显示（见 vendorShown），
               否则收起之后看不见自己选了什么 -->
          <button
            v-if="cat.vendorFacet.length > VENDOR_HEAD"
            class="chipf"
            id="model-vendor-more"
            @click="vendorsExpanded = !vendorsExpanded"
          >
            {{ vendorsExpanded ? '收起' : `展开全部 ${cat.vendorFacet.length} 家` }}
          </button>
        </div>

        <div class="facet">
          <div class="facet-l">定价类型</div>
          <button
            class="chipf"
            :class="{ on: cat.unit === '' }"
            :data-munit="''"
            @click="cat.setUnit('')"
          >
            全部模型 <span class="badge">{{ cat.whole }}</span>
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

        <div class="facet" v-if="cat.endpointFacet.length > 0">
          <div class="facet-l">端点类型</div>
          <button
            class="chipf"
            :class="{ on: cat.endpoints.length === 0 }"
            data-facet-endpoint=""
            @click="cat.endpoints.length > 0 && resetEndpoints()"
          >
            全部类型 <span class="badge">{{ cat.endpointFacet.length }}</span>
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
      </div>

      <!-- ⚠️ #model-q 必须留在结果区**之外**：它一旦落在随响应整块换掉的子树里，
           重渲染会替换输入框节点，焦点与光标随之丢失 —— 症状是"筛选框只认一个
           字符"。这坑渠道目录踩过一次，见 CatalogView 同处注释。 -->
      <div class="flex" style="gap: 7px; align-items: center; margin-bottom: 11px">
        <button
          class="btn outline sm"
          id="model-reset"
          :disabled="!cat.filtering"
          @click="resetFilters"
        >
          重置筛选
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

      <!-- 卡片模式：名字 / 发行方 / 口径 / 价格区间 / 渠道数一眼看全，明细进抽屉。
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
          <code class="mcard-n">{{ m.model_name }}</code>
          <div class="mcard-b" v-if="vendorsOf(m).length > 0">
            <span v-for="v in vendorsOf(m)" :key="v" class="badge acc">{{ v }}</span>
          </div>
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
              <th data-col="channels" class="n">渠道数</th>
              <th data-col="risk">其中</th>
              <th data-col="units">定价类型</th>
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
                <span v-for="v in vendorsOf(m)" :key="v" class="badge acc">{{ v }}</span>
                <span v-if="vendorsOf(m).length === 0" class="dim">未声明</span>
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
        <div class="flex" style="gap: 4px; flex-wrap: wrap; margin: 10px 0">
          <span v-for="v in vendorsOf(drawerModel)" :key="v" class="badge acc">{{ v }}</span>
          <span v-for="e in endpointsOf(drawerModel)" :key="e" class="badge">{{
            endpointLabel(e)
          }}</span>
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
