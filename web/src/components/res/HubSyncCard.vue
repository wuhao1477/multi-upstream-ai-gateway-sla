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
import UiField from '@/components/ui/UiField.vue'
import UiStat from '@/components/ui/UiStat.vue'
import * as adminApi from '@/api/admin'
import type { HubSyncConfig } from '@/api/types'
import { useChannelsStore } from '@/stores/channels'
import { useResourcesStore } from '@/stores/resources'
import { useToastStore } from '@/stores/toast'
import { fmtTime } from '@/utils/format'

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

/** 上一轮结果里"这次到底落没落库" —— report 模式下的数字不代表已经建了渠道。 */
const lastApplied = computed(() => cfg.value?.last_result?.applied === true)

onMounted(() => {
  void load()
})

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
    await load()
  } catch (e) {
    toast.fail('同步失败', e)
    await load()
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
      <button class="btn outline sm" id="btn-hubsync-reload" @click="load()">刷新</button>
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
      <UiField label="定时开关" for="hs-enabled">
        <!-- 用勾选框而不是两个 option 的下拉：`<option :value="false">` 渲染出的
             value 属性并不是 "false"（Vue 把真值挂在 option._value 上），
             于是它在 DOM 层面选不中，自动化点它会静默失败。 -->
        <input id="hs-enabled" v-model="enabled" type="checkbox" />
      </UiField>
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

    <template v-if="loaded && cfg !== null">
      <div class="sec-t">上次同步</div>
      <p v-if="cfg.last_run_at === undefined" class="dim" id="hubsync-last">还没有跑过</p>
      <p v-else-if="(cfg.last_error ?? '') !== ''" class="bad" id="hubsync-last">
        {{ fmtTime(cfg.last_run_at) }} 失败：{{ cfg.last_error }}
      </p>
      <template v-else>
        <p class="dim" id="hubsync-last">
          {{ fmtTime(cfg.last_run_at) }} 成功 ——
          <b>{{ lastApplied ? '已落库' : '只比对，未落库' }}</b>
        </p>
        <div class="stats" v-if="cfg.last_result !== undefined">
          <UiStat label="备份条目" :value="cfg.last_result.total" />
          <UiStat :label="lastApplied ? '已入库' : '可入库'" :value="cfg.last_result.imported" />
          <UiStat label="跳过" :value="cfg.last_result.skipped" />
          <UiStat label="失败" :value="cfg.last_result.failed" />
          <UiStat label="站型声明不符" :value="cfg.last_result.family_mismatches" />
          <UiStat label="有人机验证" :value="cfg.last_result.shielded_sites" />
          <UiStat label="缺凭证" :value="cfg.last_result.without_credential" />
        </div>
      </template>
    </template>
  </UiCard>
</template>
