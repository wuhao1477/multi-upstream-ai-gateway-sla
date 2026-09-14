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
 */
import { onMounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import UiCard from '@/components/ui/UiCard.vue'
import UiEmpty from '@/components/ui/UiEmpty.vue'
import UiPrice from '@/components/ui/UiPrice.vue'
import UiStat from '@/components/ui/UiStat.vue'
import { useGlobalCatalogStore } from '@/stores/globalCatalog'
import { fmtAgo, fmtTime, statusLabel, unitChip } from '@/utils/format'

const route = useRoute()
const router = useRouter()
const cat = useGlobalCatalogStore()

/** 输入框自己的值。理由同 CatalogView：直接绑 store.q 会在防抖窗口内被回写。 */
const qInput = ref('')
/** 展开的模型名。允许多个 —— 比较两个模型的渠道分布是常见动作。 */
const expanded = ref<Set<string>>(new Set())

function toggle(name: string): void {
  const next = new Set(expanded.value)
  if (next.has(name)) next.delete(name)
  else next.add(name)
  expanded.value = next
}

/** 把当前筛选写回 URL。replace 而非 push：每敲一个字符都进历史栈会废掉返回键。 */
function syncURL(): void {
  const q: Record<string, string> = {}
  if (cat.q !== '') q.q = cat.q
  if (cat.unit !== '') q.unit = cat.unit
  void router.replace({ query: q })
}

onMounted(async () => {
  await cat.open()
  // URL 优先于 store 的初始值：别人发来的链接必须能覆盖。open() 里重置过筛选，
  // 所以这里只在 URL 真带了参数时再设一次，避免多打一次空请求。
  const s = (k: string): string => (typeof route.query[k] === 'string' ? String(route.query[k]) : '')
  const urlQ = s('q')
  const urlUnit = s('unit')
  if (urlUnit !== '') cat.setUnit(urlUnit)
  if (urlQ !== '') {
    qInput.value = urlQ
    cat.setQuery(urlQ)
  }
})

watch([() => cat.q, () => cat.unit], syncURL)
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

      <div class="tw" v-if="cat.items.length > 0">
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
                    <div class="tw">
                      <table :data-model-channels="m.model_name">
                        <thead>
                          <tr>
                            <th>渠道</th>
                            <th>输入价</th>
                            <th>输出价</th>
                            <th>渠道状态</th>
                            <th>目录状态</th>
                            <th>最近出现</th>
                          </tr>
                        </thead>
                        <tbody>
                          <tr v-for="c in m.channels" :key="c.channel_id">
                            <td>
                              <router-link
                                class="linkish"
                                :data-model-ch="c.channel_id"
                                :to="{ name: 'channel-detail', params: { id: String(c.channel_id) } }"
                                >{{ c.channel_name }}</router-link
                              >
                            </td>
                            <td><UiPrice :value="c.input_price" :unit="c.billing_unit" /></td>
                            <td><UiPrice :value="c.output_price" :unit="c.billing_unit" /></td>
                            <td>
                              <span
                                class="badge"
                                :class="c.channel_status === 'disabled' ? 'bad' : 'ok'"
                                >{{ statusLabel(c.channel_status) }}</span
                              >
                            </td>
                            <td>
                              <span class="badge" :class="c.stale ? 'warn' : ''">{{
                                c.stale ? '疑似下架' : '在架'
                              }}</span>
                            </td>
                            <td class="dim" :title="fmtTime(c.last_seen_at)">
                              {{ fmtAgo(c.last_seen_at) }}
                            </td>
                          </tr>
                        </tbody>
                      </table>
                    </div>
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
  </div>
</template>
