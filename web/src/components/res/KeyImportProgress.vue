<script setup lang="ts">
/**
 * 后台「同步已有 Key」批次的进度卡。
 *
 * 为什么要有这张卡：同步改成后台批次之后，点完「开始同步」抽屉就关了 ——
 * 没有这张卡的话，那批任务在界面上**不存在**，人只能靠刷新 Key 列表猜它跑完没有。
 *
 * ⚠️ 队列只在 sla-core 的内存里（服务端 key_import_queue.go 写了取舍）。
 * 进程重启会让批次连同进度一起消失，轮询拿到 404 —— 这一条必须**说出来**，
 * 不能让进度条静静停在 3/40：那看起来像卡住了，而事实是它已经不在了。
 */
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import UiCard from '@/components/ui/UiCard.vue'
import UiStat from '@/components/ui/UiStat.vue'
import * as adminApi from '@/api/admin'
import { ApiError } from '@/api/client'
import type { KeyImportJob } from '@/api/types'
import { useChannelsStore } from '@/stores/channels'
import { useResourcesStore } from '@/stores/resources'
import { fmtAgo, fmtTime } from '@/utils/format'

const props = defineProps<{ jobId: number | null }>()
const emit = defineEmits<{ close: []; changed: [] }>()

const channels = useChannelsStore()
const res = useResourcesStore()

const job = ref<KeyImportJob | null>(null)
const gone = ref('')
/** 展开逐账号明细。默认收起：一批可能有两百行，而多数时候只关心汇总。 */
const detailed = ref(false)

/**
 * 轮询间隔。2 秒是照着"一个账号一般几秒"来的 —— 再快只是空转，
 * 再慢会让进度条看起来一跳一跳的。
 */
const POLL_MS = 2000
let timer: ReturnType<typeof setTimeout> | undefined
/**
 * 每次换批次或卸载 +1。clearTimeout 清不掉**在途请求**回来之后新排的定时器，
 * 不比代数的话，旧批次的轮询会接着跑，与新批次交替覆盖 job（卸载后也停不下来）。
 */
let generation = 0

function stop(): void {
  generation++
  if (timer !== undefined) clearTimeout(timer)
  timer = undefined
}

async function poll(gen = generation): Promise<void> {
  const id = props.jobId
  if (id === null) return
  try {
    const got = await adminApi.keyImportJob(id)
    if (gen !== generation) return
    // 每收一条新结果就让外面刷一次 Key 列表：这批任务的产出就是那张表，
    // 等到跑完再刷的话，前面十分钟里看到的都是旧数据。
    const advanced = job.value !== null && got.done > job.value.done
    job.value = got
    if (advanced) emit('changed')
    if (got.status === 'running') {
      timer = setTimeout(() => void poll(gen), POLL_MS)
      return
    }
    emit('changed')
  } catch (error) {
    if (gen !== generation) return
    // 404 = 这个批次不在内存里了（进程重启过，或被更近的批次挤出历史）。
    // 它与"接口坏了"是两回事，文案必须分开 —— 前者要人重新点一次同步。
    gone.value =
      error instanceof ApiError && error.status === 404
        ? error.message
        : `读取批次进度失败：${error instanceof Error ? error.message : String(error)}`
    job.value = null
  }
}

watch(
  () => props.jobId,
  (id) => {
    stop()
    job.value = null
    gone.value = ''
    detailed.value = false
    if (id !== null) void poll()
  },
  { immediate: true },
)

onBeforeUnmount(stop)

const running = computed(() => job.value?.status === 'running')
const statusLabel = computed(() => {
  const s = job.value?.status
  if (s === 'running') return '进行中'
  if (s === 'canceled') return '已中断'
  return '已完成'
})
/** 进行中用主色、被打断用红、正常结束按有没有失败账号着色。 */
const statusClass = computed(() => {
  const j = job.value
  if (j === null) return ''
  if (j.status === 'running') return 'acc'
  if (j.status === 'canceled') return 'bad'
  return j.failed_accounts > 0 ? 'warn' : 'ok'
})

function channelName(id: number): string {
  return channels.list.find((c) => c.id === id)?.name ?? `渠道 #${id}`
}

/** 只有出了状况的账号值得默认展示：一批 40 个站，全列出来找不到该修哪个。 */
const trouble = computed(() =>
  (job.value?.items ?? []).filter((i) => i.status !== 'ok' && i.status !== 'skipped'),
)
const shown = computed(() => (detailed.value ? (job.value?.items ?? []) : trouble.value))

const itemClass: Record<string, string> = {
  ok: 'ok',
  partial: 'warn',
  deferred: 'warn',
  skipped: '',
  failed: 'bad',
}
const itemLabel: Record<string, string> = {
  ok: '成功',
  partial: '部分失败',
  deferred: '稍后再来',
  skipped: '已跳过',
  failed: '失败',
}
</script>

<template>
  <UiCard v-if="jobId !== null" id="key-import-progress">
    <template #header>
      <div>
        <h2 class="card-t">后台同步已有 Key</h2>
        <p class="card-d">
          批次 #{{ jobId }}<template v-if="job !== null">
            · 开始于 {{ fmtAgo(job.started_at) }}</template
          >
        </p>
      </div>
      <span class="badge" :class="statusClass" id="key-import-status">{{ statusLabel }}</span>
      <div class="spacer"></div>
      <button
        v-if="job !== null && job.items.length > 0"
        class="btn outline sm"
        id="btn-key-import-detail"
        @click="detailed = !detailed"
      >
        {{ detailed ? '只看有问题的' : `看全部 ${job.count} 个账号` }}
      </button>
      <!-- 只在跑完之后给「收起」：跑到一半关掉这张卡，那批任务就又看不见了 -->
      <button
        v-if="!running"
        class="btn outline sm"
        id="btn-key-import-dismiss"
        @click="emit('close')"
      >
        收起
      </button>
    </template>

    <p v-if="gone !== ''" class="bad" id="key-import-gone">{{ gone }}</p>

    <template v-if="job !== null">
      <p class="note" id="key-import-progress-line">
        已处理 <b>{{ job.done }} / {{ job.total }}</b> 个账号<template v-if="job.pending > 0"
          >，队列里还有 {{ job.pending }} 个</template
        ><template v-if="job.next_retry_at !== undefined">
          —— 下一个在 {{ fmtTime(job.next_retry_at) }} 轮到（上游限流，正在退避等待）</template
        >。
      </p>
      <p v-if="job.error !== undefined && job.error !== ''" class="bad" id="key-import-error">
        {{ job.error }}
      </p>

      <!-- 两组计数分开摆，标签里写清单位。合成一组会让"6 个站连不上"被读成
           "失败 2 把"（实测撞到过，见 api/types.ts 的注释）。 -->
      <div class="sec-t">账号（个）</div>
      <div class="stats">
        <UiStat label="已处理" :value="`${job.done} / ${job.total}`" small />
        <UiStat label="整站失败" :value="job.failed_accounts" />
        <UiStat label="稍后再来" :value="job.deferred_accounts" />
        <UiStat label="已跳过（停用）" :value="job.skipped_accounts" />
      </div>
      <div class="sec-t">Key（把）</div>
      <div class="stats">
        <UiStat label="发现" :value="job.found" />
        <UiStat label="新登记" :value="job.imported" />
        <UiStat label="已有（刷新额度）" :value="job.skipped" />
        <UiStat label="失败" :value="job.failed" />
        <UiStat label="待重试" :value="job.deferred" />
      </div>

      <p class="note">
        「整站失败」要人去修（多半是采集凭证失效）；「稍后再来」是上游那 20 次 / 20
        分钟的明文读取窗口用完了，过一会儿再同步一次就行，<b>不用改任何东西</b>。
        两者要做的事不同，所以分成两个数。
      </p>

      <div class="tw" v-if="shown.length > 0">
        <table v-cell-label id="key-import-items">
          <thead>
            <tr>
              <th data-col="channel">渠道</th>
              <th data-col="account">账号</th>
              <th data-col="status">结果</th>
              <th data-col="keys" class="n">Key（发现 / 新登记 / 已有）</th>
              <th data-col="note">说明</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="i in shown" :key="`${i.channel_id}-${i.account_id}`">
              <td data-col="channel">{{ channelName(i.channel_id) }}</td>
              <td data-col="account" class="dim">{{ res.accountLabel(i.account_id) }}</td>
              <td data-col="status">
                <span class="badge" :class="itemClass[i.status] ?? ''">{{
                  itemLabel[i.status] ?? i.status
                }}</span>
                <!-- 重试过就说出来：只显示最后那次的结果会让"试了三次才成"
                     与"一次就成"看起来一模一样 -->
                <span v-if="(i.attempts ?? 1) > 1" class="dim cell-sub"
                  >第 {{ i.attempts }} 次尝试</span
                >
              </td>
              <td data-col="keys" class="n">{{ i.found }} / {{ i.imported }} / {{ i.skipped }}</td>
              <td data-col="note" class="dim">{{ i.error ?? '' }}</td>
            </tr>
          </tbody>
        </table>
      </div>
      <p class="note" v-else-if="!running && job.count > 0">
        这一批没有需要处理的账号 —— {{ job.count }} 个全部正常。
      </p>
    </template>
    <p class="note" v-else-if="gone === ''">正在读取批次进度…</p>
  </UiCard>
</template>
