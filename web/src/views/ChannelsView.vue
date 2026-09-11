<script setup lang="ts">
/**
 * 渠道管理。
 *
 * 相对旧版的两处结构性改动：
 *
 * ① **每行可二级展开**，直接看到该渠道下的账号与 Key 概览。旧版一行只有
 *    「名称 / 站型 / 地址 / 状态」，看不出这个渠道到底有没有东西 —— 要判断
 *    "它是空的还是采失败了"，得逐个点进详情。展开区的数据来自共享 store
 *    的一次全量拉取，展开不发请求。
 *
 * ② **「启用状态」与「采集健康」分成两列**。旧版只有一个 status 徽标，而
 *    "渠道是 enabled" 和 "这渠道根本采不了（没凭证 / 站型未识别）" 是两回事，
 *    后者才是最常见的"为什么不工作"。
 *
 * 新建渠道从常驻表单挪进抽屉：65 个渠道的日常操作是查与改，而建渠道一周
 * 一次 —— 让它常年占着首屏是本末倒置。
 */
import { computed, ref } from 'vue'
import { useRouter } from 'vue-router'
import UiCard from '@/components/ui/UiCard.vue'
import UiEmpty from '@/components/ui/UiEmpty.vue'
import UiField from '@/components/ui/UiField.vue'
import UiDrawer from '@/components/ui/UiDrawer.vue'
import UiConfirm from '@/components/ui/UiConfirm.vue'
import AccountTable from '@/components/res/AccountTable.vue'
import KeyTable from '@/components/res/KeyTable.vue'
import { useChannelsStore } from '@/stores/channels'
import { useResourcesStore } from '@/stores/resources'
import { useToastStore } from '@/stores/toast'
import type { Channel } from '@/api/types'
import { totalBalance, totalQuota, usd, usdFine } from '@/utils/money'
import { familyLabel, fmtAgo, statusLabel } from '@/utils/format'

const router = useRouter()
const channels = useChannelsStore()
const res = useResourcesStore()
const toast = useToastStore()

const creating = ref(false)
const showCreate = ref(false)
const name = ref('')
const url = ref('')
const family = ref('')

/**
 * 行内编辑与旧版一样保留：65 个渠道要连着改几个，弹窗每次都得关掉再点下一行。
 * 同时只能开一个 —— 两处待保存的输入会让人不知道刚才那个存了没有。
 */
const editing = ref<number | null>(null)
const editName = ref('')
const editURL = ref('')
const busy = ref(false)
/** 待确认的停用。 */
const pending = ref<Channel | null>(null)
const disableReason = ref('')
/** 展开的渠道 id。允许多个同时展开 —— 对比两个渠道是常见动作。 */
const expanded = ref<Set<number>>(new Set())

async function reloadAll(): Promise<void> {
  await Promise.all([channels.load(), res.reload()])
}

function toggle(id: number): void {
  const next = new Set(expanded.value)
  if (next.has(id)) next.delete(id)
  else next.add(id)
  expanded.value = next
}

/** 收起点中的那个「更多」菜单。原生 <details> 选完不会自己关。 */
function closeMenu(e: MouseEvent): void {
  ;(e.target as HTMLElement | null)?.closest('details')?.removeAttribute('open')
}

function startEdit(c: Channel): void {
  editing.value = c.id
  editName.value = c.name
  editURL.value = c.base_url
}

async function saveEdit(id: number): Promise<void> {
  if (editName.value.trim() === '' || editURL.value.trim() === '') {
    toast.show('名称与地址都不能为空', 'bad')
    return
  }
  busy.value = true
  try {
    const ok = await channels.patch(id, {
      name: editName.value.trim(),
      base_url: editURL.value.trim(),
    })
    if (ok) {
      toast.show(`渠道 #${id} 已更新`, 'ok')
      editing.value = null
    }
  } finally {
    busy.value = false
  }
}

/** 停用的影响面：这个渠道下所有账号与 Key 都会跟着不可用。 */
const pendingImpact = computed(() => {
  const c = pending.value
  if (c === null) return ''
  if (!res.loaded) return '停用后该渠道不再参与采集与记账。'
  const accs = res.channelAccounts(c.id)
  const ks = res.channelKeys(c.id)
  if (accs.length === 0 && ks.length === 0) {
    return '该渠道下还没有账号与 Key，停用不影响任何资源。'
  }
  return `将影响该渠道下 ${accs.length} 个账号、${ks.length} 把 Key —— 它们不再参与采集与记账。`
})

/** 停用：原因必填（FR-095）。服务端也会拦，这里先拦是为了少一次往返。 */
async function confirmDisable(): Promise<void> {
  const c = pending.value
  if (c === null) return
  if (disableReason.value.trim() === '') {
    toast.show('停用必须填原因（FR-095）', 'bad')
    return
  }
  busy.value = true
  try {
    const ok = await channels.patch(c.id, {
      status: 'disabled',
      disabled_reason: disableReason.value.trim(),
    })
    if (ok) {
      toast.show(`渠道 #${c.id} 已停用`, 'ok')
      pending.value = null
    }
  } finally {
    busy.value = false
  }
}

async function enable(id: number): Promise<void> {
  busy.value = true
  try {
    // 不传 disabled_reason：服务端在 status='enabled' 时会把原因与有效期一并清空。
    const ok = await channels.patch(id, { status: 'enabled' })
    if (ok) toast.show(`渠道 #${id} 已启用`, 'ok')
  } finally {
    busy.value = false
  }
}

async function create(): Promise<void> {
  if (name.value.trim() === '' || url.value.trim() === '') {
    toast.show('渠道名称与站点地址必填', 'bad')
    return
  }
  creating.value = true
  try {
    const ok = await channels.create({
      name: name.value.trim(),
      base_url: url.value.trim(),
      site_family: family.value,
      // 站型留空 = 自动探测
      auto_detect: family.value === '',
    })
    if (ok) {
      name.value = ''
      url.value = ''
      showCreate.value = false
    }
  } finally {
    creating.value = false
  }
}

async function open(id: number): Promise<void> {
  await router.push({ name: 'channel-detail', params: { id: String(id) } })
}

/** 跳到 Key 管理页并带上这个渠道的筛选 —— 不用到那边再选一次。 */
function openKeys(id: number): void {
  void router.push({ name: 'keys', query: { channel: String(id) } })
}

/**
 * 资金概览的一行文案。
 *
 * 刻意**不给一个数**：这里同时有两个不可相加的口径（账号余额、Key 配额），
 * 外加"未采到"这一档。压成单个数字必然要把未采到当 0，那是错的。
 */
function fundsSummary(id: number): string {
  if (!res.loaded) return '—'
  const accs = res.channelAccounts(id)
  const ks = res.channelKeys(id)
  if (accs.length === 0) return '无账号'
  const b = totalBalance(accs)
  const q = totalQuota(ks)
  const parts: string[] = []
  parts.push(b.wallets === 0 ? '余额未采集' : `账号余额 ${usd(b.total)}`)
  if (b.unknown > 0 && b.wallets > 0) parts.push(`${b.unknown} 个未采集`)
  if (q.counted > 0) parts.push(`Key 配额 ${usdFine(q.remain)}`)
  if (q.exhausted > 0) parts.push(`${q.exhausted} 把耗尽`)
  else if (q.low > 0) parts.push(`${q.low} 把将耗尽`)
  return parts.join(' · ')
}

/** 该渠道有没有阻断性问题（没凭证 / 站型未识别）。列表上要能一眼看出。 */
function blocked(c: Channel): string {
  if (c.site_family === 'unknown') return '站型未识别'
  if (res.loaded && res.channelAccounts(c.id).length === 0) return '无账号'
  return ''
}
</script>

<template>
  <div class="pane on" id="pane-channels">
    <UiCard>
      <template #header>
        <div>
          <h2 class="card-t">渠道列表</h2>
          <p class="card-d">上游服务入口。一个渠道可挂多个账号，一个账号可挂多把 Key。</p>
        </div>
        <span class="badge" id="ch-count">{{
          channels.loaded ? `共 ${channels.count} 个` : '—'
        }}</span>
        <div class="spacer"></div>
        <div class="tb-search">
          <label class="sr" for="ch-filter">筛选渠道</label>
          <input id="ch-filter" v-model="channels.filter" placeholder="按名称 / 地址 / 站型筛选" />
        </div>
        <!-- 刷新连账号与 Key 一起拉：这张表的三列读的是它们，
             只刷渠道会得到一张"渠道更新了、资产还是旧的"的表 -->
        <button class="btn outline sm" id="btn-reload" @click="reloadAll">刷新</button>
        <button class="btn" id="btn-open-create" @click="showCreate = true">+ 新建渠道</button>
      </template>

      <div id="channels">
        <UiEmpty v-if="!channels.loaded">填入令牌后点「刷新」</UiEmpty>
        <UiEmpty v-else-if="channels.count === 0">还没有渠道，点右上角「新建渠道」</UiEmpty>
        <UiEmpty v-else-if="channels.filtered.length === 0">
          没有匹配「{{ channels.filter.trim() }}」的渠道
        </UiEmpty>
        <template v-else>
          <div class="tw">
            <table class="ch-table">
              <thead>
                <!-- 表头带 data-col 且与 <td> 一一对应：窄屏成对隐藏列，
                     只给 td 加会让整行右移一格（见 KeyTable 同处注释）。 -->
                <tr>
                  <th class="x"></th>
                  <th data-col="name">渠道</th>
                  <th data-col="family">站型</th>
                  <th data-col="accounts" class="n">账号</th>
                  <th data-col="keys" class="n">Key</th>
                  <th data-col="funds">资金概览</th>
                  <th data-col="synced">最近同步</th>
                  <!-- 「启用状态」而不是笼统的「状态」：它说的是"人有没有把它关掉"。
                       "能不能采集"是另一回事，落在站型那一列的红徽标与资产总览的
                       异常项上 —— 两者原先都叫「状态」，于是一个 enabled 但根本
                       采不了的渠道看起来完全正常 -->
                  <th data-col="status">启用状态</th>
                  <th data-col="acts"></th>
                </tr>
              </thead>
              <tbody>
                <template v-for="c in channels.filtered" :key="c.id">
                  <!-- data-ch-row 标出**数据行**。编辑/展开时会往 tbody 里插一个
                       sub-row，而验收脚本按 tr[data-ch-row] 数渠道数 ——
                       不区分的话，展开着一行时数出来会多一个。 -->
                  <tr :data-ch-row="c.id" :class="{ open: expanded.has(c.id) }">
                    <td class="x">
                      <button
                        class="twist"
                        :data-ch-toggle="c.id"
                        :aria-expanded="expanded.has(c.id)"
                        :aria-label="`展开渠道 ${c.name} 的资产`"
                        @click="toggle(c.id)"
                      >
                        {{ expanded.has(c.id) ? '▾' : '▸' }}
                      </button>
                    </td>
                    <td data-col="name">
                      <button class="linkish strong" :data-ch-name="c.id" :data-ch="c.id" @click="open(c.id)">
                        {{ c.name }}
                      </button>
                      <span class="dim cell-sub" :title="c.base_url">{{ c.base_url }}</span>
                    </td>
                    <td data-col="family">
                      <span class="badge">{{ familyLabel(c.site_family) }}</span>
                      <!-- 阻断性问题就摆在站型旁边：站型未识别 = 没有适配器可用，
                           这个渠道无论如何都采不到任何东西（04 §7） -->
                      <span v-if="blocked(c) !== ''" class="badge bad" :data-ch-blocked="c.id">{{
                        blocked(c)
                      }}</span>
                    </td>
                    <td data-col="accounts" class="n">
                      {{ res.loaded ? res.channelAccounts(c.id).length : '—' }}
                    </td>
                    <td data-col="keys" class="n">
                      {{ res.loaded ? res.channelKeys(c.id).length : '—' }}
                    </td>
                    <td data-col="funds" class="dim">{{ fundsSummary(c.id) }}</td>
                    <td data-col="synced" class="dim">
                      {{
                        res.loaded
                          ? fmtAgo(
                              res
                                .channelKeys(c.id)
                                .map((k) => k.quota_synced_at)
                                .filter((t): t is string => t !== undefined)
                                .sort()
                                .pop(),
                            )
                          : '—'
                      }}
                    </td>
                    <td :data-ch-status="c.id" data-col="status">
                      <span class="badge" :class="c.status === 'enabled' ? 'ok' : 'bad'">{{
                        statusLabel(c.status)
                      }}</span>
                      <!-- 停用原因就显示在状态旁边：停用是要人来解除的，
                           看不到原因就解不了（FR-095） -->
                      <span
                        v-if="c.status === 'disabled' && c.disabled_reason !== undefined"
                        class="dim cell-sub"
                        :data-ch-reason="c.id"
                      >
                        {{ c.disabled_reason }}
                      </span>
                    </td>
                    <td data-col="acts" class="acts">
                      <button class="btn ghost sm" :data-ch-detail="c.id" @click="open(c.id)">
                        详情
                      </button>
                      <details class="more">
                        <summary class="btn ghost sm" :data-ch-more="c.id">更多</summary>
                        <div class="more-menu" @click="closeMenu">
                          <button class="btn ghost sm" :data-ch-edit="c.id" @click="startEdit(c)">
                            编辑
                          </button>
                          <button class="btn ghost sm" @click="openKeys(c.id)">查看全部 Key</button>
                          <button
                            v-if="c.status === 'enabled'"
                            class="btn ghost sm danger-t"
                            :data-ch-disable="c.id"
                            @click="pending = c; disableReason = ''"
                          >
                            停用
                          </button>
                          <button
                            v-else
                            class="btn ghost sm"
                            :data-ch-enable="c.id"
                            :disabled="busy"
                            @click="enable(c.id)"
                          >
                            启用
                          </button>
                        </div>
                      </details>
                    </td>
                  </tr>

                  <!-- 编辑行。站型不给改：它由 Detect 判定，手改会让字段映射全错 -->
                  <tr v-if="editing === c.id" class="sub-row">
                    <td colspan="9">
                      <div class="row-form">
                        <UiField label="渠道名称" for="ch-edit-name">
                          <input id="ch-edit-name" v-model="editName" />
                        </UiField>
                        <UiField label="站点地址" for="ch-edit-url">
                          <input id="ch-edit-url" v-model="editURL" />
                        </UiField>
                        <div class="endcap">
                          <button
                            class="btn sm"
                            id="btn-ch-save"
                            :disabled="busy"
                            @click="saveEdit(c.id)"
                          >
                            保存
                          </button>
                          <button class="btn outline sm" id="btn-ch-cancel" @click="editing = null">
                            取消
                          </button>
                        </div>
                      </div>
                      <p class="note">
                        站型由自动探测判定，此处不提供修改 —— 它决定用哪个适配器、带哪个用户 ID
                        头、怎么解析分页，手改会让整套字段映射错位。
                      </p>
                    </td>
                  </tr>

                  <!-- 二级展开：账号 + Key 概览。**不再往下嵌第三层表格** ——
                       账号下的 Key 在账号页展开看，这里只给概览与去处 -->
                  <tr v-if="expanded.has(c.id)" class="sub-row">
                    <td colspan="9">
                      <div class="expand">
                        <section>
                          <div class="sec-t">
                            账号
                            <span class="badge">{{ res.channelAccounts(c.id).length }} 个</span>
                          </div>
                          <AccountTable
                            :items="res.channelAccounts(c.id)"
                            :expandable="false"
                            compact
                            empty-text="该渠道还没有账号 —— 没有账号就没有 Key，也采不到余额"
                          />
                        </section>
                        <section>
                          <div class="sec-t">
                            Key
                            <span class="badge">{{ res.channelKeys(c.id).length }} 把</span>
                            <div class="spacer"></div>
                            <!-- 「查看全部」带上渠道筛选跳过去，而不是让人到那边再选一次 -->
                            <button class="linkish" :data-ch-allkeys="c.id" @click="openKeys(c.id)">
                              在 Key 管理中查看全部 →
                            </button>
                          </div>
                          <KeyTable
                            :items="res.channelKeys(c.id).slice(0, 5)"
                            compact
                            empty-text="该渠道还没有登记 Key"
                          />
                          <p class="note" v-if="res.channelKeys(c.id).length > 5">
                            只显示前 5 把（共 {{ res.channelKeys(c.id).length }} 把）。
                            展开区是速览，不在这里加载几百行。
                          </p>
                        </section>
                      </div>
                    </td>
                  </tr>
                </template>
              </tbody>
            </table>
          </div>
          <p class="note" v-if="channels.filter.trim() !== ''">
            筛选出 {{ channels.filtered.length }} / {{ channels.count }} 个
          </p>
        </template>
      </div>
    </UiCard>

    <UiDrawer
      :open="showCreate"
      title="新建渠道"
      desc="自动探测会请求站点的公开端点判定家族（04 §2），探测失败不影响创建"
      @close="showCreate = false"
    >
      <UiField label="渠道名称" for="ch-name">
        <input id="ch-name" v-model="name" placeholder="例：某中转站-主账号" />
      </UiField>
      <UiField label="站点地址 base_url" for="ch-url">
        <input id="ch-url" v-model="url" placeholder="https://api.example.com" />
      </UiField>
      <UiField label="站型家族" for="ch-family">
        <!--
          选项来自后端注册表（GET /admin/site-families），不写死。
          写死过一次：加站型时这份列表不会报任何错，新站型只是在界面上
          不存在，运维只能靠自动探测碰上它。
          "未知"不在注册表里也不该在这里 —— 它是探测未命中的哨兵，
          手选它等于建一个必然采不了的渠道（04 §7）。
        -->
        <select id="ch-family" v-model="family">
          <option value="">自动探测（推荐）</option>
          <option v-for="f in channels.families" :key="f.family" :value="f.family">
            {{ f.display_name }}
          </option>
        </select>
      </UiField>
      <p class="note">
        建完渠道还差两步才能采到数据：登记<b>采集凭证</b>（否则采集必然失败），
        以及登记<b>账号</b>与 <b>Key</b>。资产总览会把缺哪一步直接标出来。
      </p>
      <template #footer>
        <button class="btn" id="btn-create" :disabled="creating" @click="create">创建渠道</button>
        <button class="btn outline" @click="showCreate = false">取消</button>
      </template>
    </UiDrawer>

    <UiConfirm
      :open="pending !== null"
      title="停用这个渠道？"
      :impact="pendingImpact"
      confirm-text="确认停用"
      :busy="busy"
      @cancel="pending = null"
      @confirm="confirmDisable"
    >
      <UiField label="停用原因（必填，FR-095）" for="ch-dis-reason">
        <input
          id="ch-dis-reason"
          v-model="disableReason"
          placeholder="例：站点跑路 / 余额耗尽待充值"
        />
      </UiField>
    </UiConfirm>
  </div>
</template>
