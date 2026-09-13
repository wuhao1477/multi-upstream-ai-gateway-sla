<script setup lang="ts">
/**
 * all-api-hub 的 WebDAV 定时同步。
 *
 * 和上面那张卡的区别：那张是人手工上传一份导出文件，这张是网关自己按周期去
 * WebDAV 上取同一份东西。扩展那边开了「备份加密」的话取回来的是个信封
 * （PBKDF2-SHA256 + AES-256-GCM），解密在服务端做，密码存库不回显。
 *
 * 两个密码框每次打开都是空的 —— 接口只回 has_*。所以**空 = 不改**，不是清空；
 * 否则任何一次"只改个间隔"的保存都会顺手把密码抹掉，而症状要等下一轮 401。
 */
import { computed, onMounted, ref } from 'vue'
import UiCard from '@/components/ui/UiCard.vue'
import UiEmpty from '@/components/ui/UiEmpty.vue'
import UiField from '@/components/ui/UiField.vue'
import UiStat from '@/components/ui/UiStat.vue'
import * as adminApi from '@/api/admin'
import type { HubSyncConfig, HubSyncRun } from '@/api/types'
import { useChannelsStore } from '@/stores/channels'
import { useResourcesStore } from '@/stores/resources'
import { useToastStore } from '@/stores/toast'
import { declaredFamilyLabel, familyLabel, fmtTime, importStatusLabel } from '@/utils/format'

const channels = useChannelsStore()
const res = useResourcesStore()
const toast = useToastStore()

const cfg = ref<HubSyncConfig | null>(null)
const loaded = ref(false)
const busy = ref(false)

const url = ref('')
const username = ref('')
const password = ref('')
const backupPassword = ref('')
const enabled = ref(false)
const interval = ref(360)
const applyMode = ref<'report' | 'import'>('report')

const runs = ref<HubSyncRun[]>([])
const runsLoaded = ref(false)
/** 打开的那条详情。null = 弹窗关着。 */
const detail = ref<HubSyncRun | null>(null)
const detailLoading = ref(false)

/** 最近一轮。列表已经是倒序，取第一条即可，不用再按时间比一次。 */
const latest = computed<HubSyncRun | undefined>(() => runs.value[0])

onMounted(() => {
  void load()
  void loadRuns()
})

async function loadRuns(): Promise<void> {
  try {
    runs.value = (await adminApi.listHubSyncRuns()).items
    runsLoaded.value = true
  } catch (e) {
    toast.fail('读取同步历史失败', e)
  }
}

/**
 * 打开一条记录的详情。
 *
 * 列表那份 result 是**剥掉 items 的**（服务端 `result - 'items'`），所以点开时
 * 必须重新取一次整条 —— 直接把列表里那条塞进弹窗的话，明细永远是空的，
 * 而弹窗看起来完全正常。
 */
async function openDetail(id: number): Promise<void> {
  detailLoading.value = true
  try {
    detail.value = await adminApi.getHubSyncRun(id)
  } catch (e) {
    toast.fail('读取同步详情失败', e)
  } finally {
    detailLoading.value = false
  }
}

/** 触发方式的中文。英文枚举直接抛给运维，得让人自己去猜 schedule 是什么。 */
function triggerLabel(v: string): string {
  return v === 'schedule' ? '定时' : '手动'
}

/** 毫秒 → 人读的耗时。上百个站点的探测动辄几十秒，纯毫秒数没法一眼比较。 */
function elapsedLabel(ms: number): string {
  if (ms < 1000) return `${ms} 毫秒`
  if (ms < 60_000) return `${(ms / 1000).toFixed(1)} 秒`
  return `${Math.floor(ms / 60_000)} 分 ${Math.round((ms % 60_000) / 1000)} 秒`
}

/** 明细行的状态 → 徽标类，与手工导入那张表同一套口径。 */
function itemClass(s: string): string {
  if (s === 'imported') return 'ok'
  if (s === 'would_import') return 'acc'
  if (s === 'failed') return 'bad'
  return ''
}

async function load(): Promise<void> {
  try {
    const d = await adminApi.getHubSync()
    cfg.value = d
    url.value = d.webdav_url
    username.value = d.webdav_username
    enabled.value = d.enabled
    interval.value = d.interval_minutes
    applyMode.value = d.apply_mode
    loaded.value = true
  } catch (e) {
    toast.fail('读取同步配置失败', e)
  }
}

async function save(): Promise<void> {
  busy.value = true
  try {
    cfg.value = await adminApi.saveHubSync({
      webdav_url: url.value.trim(),
      webdav_username: username.value.trim(),
      webdav_password: password.value,
      backup_password: backupPassword.value,
      enabled: enabled.value,
      interval_minutes: interval.value,
      apply_mode: applyMode.value,
    })
    // 秘密字段用完即清：留在输入框里等于留在 DOM 上。
    password.value = ''
    backupPassword.value = ''
    toast.show('同步配置已保存', 'ok')
  } catch (e) {
    toast.fail('保存同步配置失败', e)
  } finally {
    busy.value = false
  }
}

/**
 * 立即跑一轮。apply=true 是强制落库 —— 会真的建出渠道，所以要确认一次，
 * 与上面那张卡的「正式导入」同一条理由（不可逆、一次能建上百个）。
 */
async function runNow(apply: boolean): Promise<void> {
  if (apply && !confirm('立即导入会按远端备份建出渠道与凭证，且不可逆。确定？')) return
  busy.value = true
  try {
    const d = await adminApi.runHubSync(apply)
    toast.show(
      `${apply ? '同步并导入完成' : '同步完成（未落库）'}：` +
        `${d.total} 个条目 / ${apply ? '已入库' : '可入库'} ${d.imported} / ` +
        `跳过 ${d.skipped} / 失败 ${d.failed}`,
      'ok',
    )
    if (apply) await Promise.all([channels.load(), res.reload()])
    await Promise.all([load(), loadRuns()])
  } catch (e) {
    toast.fail('同步失败', e)
    // 失败那轮同样进历史，所以照样要重拉 —— 不拉的话界面上看不到刚发生的失败。
    await Promise.all([load(), loadRuns()])
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <UiCard id="hubsync-card">
    <template #header>
      <h2 class="card-t">All API Hub 定时同步</h2>
      <span class="badge" :class="cfg?.enabled === true ? 'ok' : ''" id="hubsync-state">{{
        loaded ? (cfg?.enabled === true ? '已开启' : '未开启') : '—'
      }}</span>
      <div class="spacer"></div>
      <button class="btn outline sm" id="btn-hubsync-reload" @click="load(); loadRuns()">
        刷新
      </button>
    </template>
    <p class="card-d">
      按周期去 WebDAV 取同一份 all-api-hub 备份，省掉每次手工导出上传。
      扩展那边开了「备份加密」的话取回的是 PBKDF2 + AES-GCM 信封，
      <b>解密在服务端做</b>，密码存库但一律不回显。
    </p>

    <div class="grid">
      <UiField label="WebDAV 地址" for="hs-url">
        <input
          id="hs-url"
          v-model="url"
          placeholder="https://dav.example/dav/ 或直接指向 .json"
          autocomplete="off"
        />
      </UiField>
      <UiField label="用户名" for="hs-user">
        <input id="hs-user" v-model="username" autocomplete="off" />
      </UiField>
      <UiField label="WebDAV 密码" for="hs-pass">
        <input
          id="hs-pass"
          v-model="password"
          type="password"
          autocomplete="off"
          :placeholder="cfg?.has_webdav_password === true ? '已配置，留空则不改' : '未配置'"
        />
      </UiField>
      <UiField label="备份解密密码" for="hs-enc">
        <input
          id="hs-enc"
          v-model="backupPassword"
          type="password"
          autocomplete="off"
          :placeholder="cfg?.has_backup_password === true ? '已配置，留空则不改' : '远端未加密则留空'"
        />
      </UiField>
      <UiField label="同步间隔（分钟）" for="hs-interval">
        <input id="hs-interval" v-model.number="interval" type="number" min="5" step="5" />
      </UiField>
      <UiField label="同步后做什么" for="hs-mode">
        <select id="hs-mode" v-model="applyMode">
          <option value="report">只比对，不落库</option>
          <option value="import">直接导入（会建渠道）</option>
        </select>
      </UiField>
      <!-- 勾选框走仓库既有的 .chk（label 包 input），不放进 UiField：
           UiField 是"标签在上、控件占满整行"的布局，而全局 `input` 规则是
           width:100%/height:36px —— 勾选框套进去会被拉成一个横贯整格的方块。
           .chk 里那条 `input { width:auto; height:auto }` 正是为此存在的。

           也不用两个 option 的下拉：`<option :value="false">` 渲染出的 value
           属性并不是 "false"（Vue 把真值挂在 option._value 上），在 DOM 层面
           选不中，自动化点它会静默失败。 -->
      <label class="chk" for="hs-enabled">
        <input id="hs-enabled" v-model="enabled" type="checkbox" />
        开启定时同步
      </label>
      <div class="endcap">
        <button class="btn" id="btn-hubsync-save" :disabled="busy" @click="save">保存配置</button>
        <button class="btn outline" id="btn-hubsync-run" :disabled="busy" @click="runNow(false)">
          立即同步（不落库）
        </button>
        <button class="btn outline" id="btn-hubsync-apply" :disabled="busy" @click="runNow(true)">
          立即同步并导入
        </button>
      </div>
    </div>
    <p class="note">
      地址填目录即可，服务端会按扩展的默认路径补出
      <code>all-api-hub-backup/all-api-hub-1-0.json</code>；填 <code>.json</code> 直链则原样用。
      间隔下限 5 分钟 —— 每轮要探测备份里的上百个站点，打太勤是在骚扰别人家站点。
    </p>

    <div class="sec-t">
      同步历史
      <span class="badge" id="hubsync-runs-count">{{
        runsLoaded ? `最近 ${runs.length} 轮` : '—'
      }}</span>
    </div>
    <UiEmpty v-if="!runsLoaded">加载中…</UiEmpty>
    <UiEmpty v-else-if="runs.length === 0" id="hubsync-runs-empty">
      还没有跑过。配好上面那几项后点「立即同步」，或等定时器到点。
    </UiEmpty>
    <div v-else class="tw" id="hubsync-runs">
      <table>
        <thead>
          <tr>
            <th>开始时间</th>
            <th>触发</th>
            <th>结果</th>
            <th>是否落库</th>
            <th class="n">条目</th>
            <th class="n">入库</th>
            <th class="n">跳过</th>
            <th class="n">失败</th>
            <th class="n">耗时</th>
          </tr>
        </thead>
        <tbody>
          <!-- 整行可点：要看的是"这一轮发生了什么"，把入口做成行尾一个小按钮
               只会让人先找那个按钮。行上给 cursor:pointer（.picker-table 同款）。 -->
          <tr
            v-for="r in runs"
            :key="r.id"
            :data-hubsync-run="r.id"
            class="clickable"
            @click="openDetail(r.id)"
          >
            <td>{{ fmtTime(r.started_at) }}</td>
            <td class="dim">{{ triggerLabel(r.trigger) }}</td>
            <td>
              <span class="badge" :class="(r.error ?? '') === '' ? 'ok' : 'bad'">{{
                (r.error ?? '') === '' ? '成功' : '失败'
              }}</span>
            </td>
            <!-- 落没落库必须单独一列：report 模式下"入库 68"其实一个渠道都没建，
                 两种轮次在数字上看起来一模一样。 -->
            <td class="dim">{{ r.applied ? '已落库' : '未落库' }}</td>
            <td class="n">{{ r.result?.total ?? '—' }}</td>
            <td class="n">{{ r.result?.imported ?? '—' }}</td>
            <td class="n">{{ r.result?.skipped ?? '—' }}</td>
            <td class="n">{{ r.result?.failed ?? '—' }}</td>
            <td class="n dim">{{ elapsedLabel(r.elapsed_ms) }}</td>
          </tr>
        </tbody>
      </table>
    </div>
    <p class="note">
      只保留最近 50 轮（写入时裁剪）。点任意一行看这一轮的逐站明细。
      <template v-if="latest !== undefined && (latest.error ?? '') !== ''">
        <b class="bad">最近一轮失败：{{ latest.error }}</b>
      </template>
    </p>

  </UiCard>

  <!-- 详情弹窗。复用 ScopePicker 那套 .drawer-scrim.center + .picker：
       它已经是"居中、限高、正文可滚"的弹窗，再写一套只会多一份要维护的 CSS。
       v-if 而不是 v-show：关掉就该从 DOM 上消失。 -->
  <div v-if="detail !== null" class="drawer-scrim center" @click.self="detail = null">
    <section class="picker" role="dialog" aria-modal="true" aria-label="同步详情" id="hubsync-detail">
      <header class="picker-h">
        <div>
          <h2 class="drawer-t">同步详情 #{{ detail.id }}</h2>
          <p class="drawer-d">
            {{ fmtTime(detail.started_at) }} 起，{{ triggerLabel(detail.trigger) }}触发，
            耗时 {{ elapsedLabel(detail.elapsed_ms) }}，
            <b>{{ detail.applied ? '已落库' : '未落库（只比对）' }}</b>
          </p>
        </div>
        <button class="btn ghost sm drawer-x" aria-label="关闭" @click="detail = null">✕</button>
      </header>

      <div class="picker-b">
        <p v-if="(detail.error ?? '') !== ''" class="bad" id="hubsync-detail-error">
          这一轮失败：{{ detail.error }}
        </p>
        <template v-if="detail.result !== undefined">
          <div class="stats">
            <UiStat label="备份条目" :value="detail.result.total" />
            <UiStat :label="detail.applied ? '已入库' : '可入库'" :value="detail.result.imported" />
            <UiStat label="跳过" :value="detail.result.skipped" />
            <UiStat label="失败" :value="detail.result.failed" />
            <UiStat label="站型声明不符" :value="detail.result.family_mismatches" />
            <UiStat label="有人机验证" :value="detail.result.shielded_sites" />
            <UiStat label="缺凭证" :value="detail.result.without_credential" />
            <UiStat label="发现密钥" :value="detail.result.keys_found" />
            <UiStat label="新导入密钥" :value="detail.result.keys_imported" />
          </div>

          <div class="tw" style="margin-top: 14px" v-if="(detail.result.items?.length ?? 0) > 0">
            <table>
              <thead>
                <tr>
                  <th>站点</th>
                  <th>地址</th>
                  <th>探测站型</th>
                  <th>结果</th>
                  <th>说明</th>
                </tr>
              </thead>
              <tbody>
                <tr v-for="(i, idx) in detail.result.items" :key="idx">
                  <td>{{ i.site_name === '' ? '—' : i.site_name }}</td>
                  <td class="dim">{{ i.site_url }}</td>
                  <td>
                    <span v-if="i.detected_family !== undefined" class="badge">{{
                      familyLabel(i.detected_family)
                    }}</span>
                    <span v-else class="dim">—</span>
                    <span
                      v-if="i.family_mismatch === true"
                      class="dim cell-sub"
                      title="导出声明与探测不符，以探测为准"
                      >声明 {{ declaredFamilyLabel(i.declared_family) }}</span
                    >
                  </td>
                  <td>
                    <span class="badge" :class="itemClass(i.status)">{{
                      importStatusLabel(i.status)
                    }}</span>
                  </td>
                  <td class="dim" style="font-size: 12px">
                    {{ i.reason ?? '' }}
                    <br v-if="i.warning !== undefined && i.reason !== undefined" />
                    <template v-if="i.warning !== undefined">⚠️ {{ i.warning }}</template>
                  </td>
                </tr>
              </tbody>
            </table>
          </div>
          <!-- 明细缺席与"这轮一个站都没有"是两回事：早期记录可能根本没存 items。 -->
          <p v-else class="note">这一轮没有逐站明细（取回或解密就失败了，还没走到探测）。</p>
        </template>
        <p v-else class="note">这一轮没有结果数据。</p>
      </div>
    </section>
  </div>
</template>
