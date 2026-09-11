<script setup lang="ts">
import { ref } from 'vue'
import UiCard from '@/components/ui/UiCard.vue'
import UiStat from '@/components/ui/UiStat.vue'
import * as adminApi from '@/api/admin'
import type { HubImportResult } from '@/api/types'
import { useChannelsStore } from '@/stores/channels'
import { useCredentialsStore } from '@/stores/credentials'
import { useResourcesStore } from '@/stores/resources'
import { useToastStore } from '@/stores/toast'
import { declaredFamilyLabel, familyLabel, importStatusLabel } from '@/utils/format'

const channels = useChannelsStore()
const creds = useCredentialsStore()
const res = useResourcesStore()
const toast = useToastStore()

const file = ref<File | null>(null)
const busy = ref(false)
const result = ref<HubImportResult | null>(null)
const wasDry = ref(false)
const error = ref('')

function pick(e: Event): void {
  const input = e.target as HTMLInputElement
  file.value = input.files?.[0] ?? null
}

async function run(dry: boolean): Promise<void> {
  if (file.value === null) {
    toast.show('先选一个备份 JSON 文件', 'bad')
    return
  }
  if (!dry && !confirm('正式导入会按备份建出渠道与凭证，且不可逆。确定？')) return

  busy.value = true
  result.value = null
  error.value = ''
  wasDry.value = dry
  try {
    // 原样上传文件文本，不在前端解析：解析规则在服务端
    // （collector.ParseHubBackup），前端再解一遍就有了第二份会漂移的实现。
    const raw = await file.value.text()
    const d = await adminApi.importHub(raw, dry)
    result.value = d
    if (!dry) await Promise.all([channels.load(), res.reload(), creds.load()])
    // 服务端 finishImport 把 would_import 也计进 imported，没有单独字段
    toast.show(
      `${dry ? '试运行完成' : '导入完成'}：${d.imported} 个${dry ? '可入库' : '已入库'} / ` +
        `跳过 ${d.skipped} / 失败 ${d.failed}`,
      'ok',
    )
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
    toast.fail('导入失败', e)
  } finally {
    busy.value = false
  }
}

/** 导入结果 → 徽标类。would_import 用 acc（蓝）：它是"可以但还没做"。 */
function statusClass(s: string): string {
  if (s === 'imported') return 'ok'
  if (s === 'would_import') return 'acc'
  if (s === 'failed') return 'bad'
  return ''
}
</script>

<template>
  <div class="pane on" id="pane-import">
    <UiCard>
      <template #header>
        <h2 class="card-t">导入 all-api-hub 备份</h2>
        <span class="badge acc">POST /admin/import/all-api-hub</span>
      </template>
      <p class="card-d">
        选备份 JSON。<b>站型以本方 Detect 为准</b>，不信任导出里的
        <code>site_type</code>（实测有站点声明与实际不符，信错会把余额/额度/模型全解析错）。
      </p>
      <div class="flex">
        <div style="flex: 1; min-width: 230px">
          <label for="imp-file">备份文件</label>
          <input
            id="imp-file"
            type="file"
            accept=".json,application/json"
            style="height: auto; padding: 7px 11px"
            @change="pick"
          />
        </div>
        <button class="btn outline" id="btn-imp-dry" :disabled="busy" @click="run(true)">
          试运行（不落库）
        </button>
        <button class="btn" id="btn-imp" :disabled="busy" @click="run(false)">正式导入</button>
      </div>
      <p class="note">
        先试运行：导入会一次建出上百个渠道，且不可逆。 同 <code>base_url</code>
        已存在的渠道会跳过，故重复导入安全。 探测上百个站点需要时间，请勿中途刷新页面。
      </p>

      <div id="imp-result">
        <p v-if="busy" class="dim" style="margin-top: 14px">
          正在探测每个站点的站型（并发 12，上百站可能要一两分钟）…
        </p>
        <p v-else-if="error !== ''" class="bad" style="margin-top: 14px">导入失败：{{ error }}</p>
        <template v-else-if="result !== null">
          <div class="sec-t">{{ wasDry ? '试运行结果（未落库）' : '导入结果' }}</div>
          <div class="stats">
            <UiStat label="备份条目" :value="result.total" />
            <UiStat :label="wasDry ? '可入库' : '已入库'" :value="result.imported" />
            <UiStat label="跳过" :value="result.skipped" />
            <UiStat label="失败" :value="result.failed" />
            <UiStat label="站型声明不符" :value="result.family_mismatches" />
            <UiStat label="有人机验证" :value="result.shielded_sites" />
            <UiStat label="缺凭证" :value="result.without_credential" />
            <UiStat label="发现密钥" :value="result.keys_found" />
            <UiStat label="新导入密钥" :value="result.keys_imported" />
            <UiStat label="已有密钥" :value="result.keys_skipped" />
            <UiStat label="密钥失败" :value="result.keys_failed" />
          </div>
          <div class="tw" style="margin-top: 14px">
            <table>
              <thead>
                <tr>
                  <th>站点</th>
                  <th>地址</th>
                  <th>探测站型</th>
                  <th>结果</th>
                  <th>密钥</th>
                  <th>说明</th>
                </tr>
              </thead>
              <tbody>
                <tr v-for="(i, idx) in result.items" :key="idx">
                  <td>{{ i.site_name === '' ? '—' : i.site_name }}</td>
                  <td class="dim">{{ i.site_url }}</td>
                  <td>
                    <span
                      v-if="i.detected_family !== undefined"
                      class="badge"
                      :class="i.family_mismatch === true ? 'warn' : ''"
                      >{{ familyLabel(i.detected_family) }}</span
                    >
                    <span v-else class="dim">—</span>
                    <span
                      v-if="i.family_mismatch === true"
                      class="dim"
                      style="font-size: 11px"
                      title="导出声明与探测不符，以探测为准"
                      >声明 {{ declaredFamilyLabel(i.declared_family) }}</span
                    >
                  </td>
                  <td>
                    <span class="badge" :class="statusClass(i.status)">{{ importStatusLabel(i.status) }}</span>
                  </td>
                  <td class="dim">
                    <template v-if="i.keys_found !== undefined">
                      发现 {{ i.keys_found }}<br />
                      新增 {{ i.keys_imported ?? 0 }} · 已有 {{ i.keys_skipped ?? 0 }}
                      <template v-if="(i.keys_failed ?? 0) > 0">
                        · 失败 {{ i.keys_failed }}
                      </template>
                    </template>
                    <span v-else>—</span>
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
          <p class="note">
            「站型声明不符」计数不是噪音：站型决定全部字段映射，
            信错导出声明会把余额、额度、模型全解析错且错得静默 —— 故一律以本方 Detect 为准。
            有人机验证的站点服务端采集不可行，需转人工录入（04 §6）。
          </p>
        </template>
      </div>
    </UiCard>
  </div>
</template>
