<script setup lang="ts">
import { ref } from 'vue'
import { useRouter } from 'vue-router'
import UiCard from '@/components/ui/UiCard.vue'
import UiEmpty from '@/components/ui/UiEmpty.vue'
import UiField from '@/components/ui/UiField.vue'
import { useChannelsStore } from '@/stores/channels'
import { useToastStore } from '@/stores/toast'

const router = useRouter()
const channels = useChannelsStore()
const toast = useToastStore()

const name = ref('')
const url = ref('')
const family = ref('')
const creating = ref(false)

/**
 * 编辑与停用都在行内展开，不用弹窗：
 * 65 个渠道要连着改几个，弹窗每次都得关掉再点下一行。
 * 展开的那一行是 null 或渠道 id —— 同时只能开一个，避免两处待保存的输入。
 */
const editing = ref<number | null>(null)
const editName = ref('')
const editURL = ref('')
const disabling = ref<number | null>(null)
const disableReason = ref('')
const busy = ref(false)

function startEdit(c: { id: number; name: string; base_url: string }): void {
  disabling.value = null
  editing.value = c.id
  editName.value = c.name
  editURL.value = c.base_url
}

function startDisable(id: number): void {
  editing.value = null
  disabling.value = id
  disableReason.value = ''
}

function cancel(): void {
  editing.value = null
  disabling.value = null
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
      cancel()
    }
  } finally {
    busy.value = false
  }
}

/** 停用：原因必填（FR-095）。服务端也会拦，这里先拦是为了少一次往返。 */
async function confirmDisable(id: number): Promise<void> {
  if (disableReason.value.trim() === '') {
    toast.show('停用必须填原因（FR-095）', 'bad')
    return
  }
  busy.value = true
  try {
    const ok = await channels.patch(id, {
      status: 'disabled',
      disabled_reason: disableReason.value.trim(),
    })
    if (ok) {
      toast.show(`渠道 #${id} 已停用`, 'ok')
      cancel()
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
    }
  } finally {
    creating.value = false
  }
}

async function open(id: number, chName: string): Promise<void> {
  await router.push({ name: 'detail' })
  await channels.select(id, chName)
}
</script>

<template>
  <div class="pane on" id="pane-channels">
    <UiCard
      title="新增渠道商"
      desc="自动探测会请求站点的公开端点判定家族（04 §2），探测失败不影响创建"
    >
      <div class="grid">
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
        <div class="endcap">
          <button class="btn" id="btn-create" :disabled="creating" @click="create">
            创建渠道
          </button>
        </div>
      </div>
    </UiCard>

    <UiCard>
      <template #header>
        <h2 class="card-t">渠道列表</h2>
        <span class="badge" id="ch-count">{{
          channels.loaded ? `共 ${channels.count} 个` : '—'
        }}</span>
        <div class="spacer"></div>
        <div style="width: 210px">
          <input id="ch-filter" v-model="channels.filter" placeholder="按名称 / 地址 / 站型筛选" />
        </div>
        <button class="btn outline sm" id="btn-reload" @click="channels.load()">刷新</button>
      </template>

      <div id="channels">
        <UiEmpty v-if="!channels.loaded">填入令牌后点「刷新」</UiEmpty>
        <UiEmpty v-else-if="channels.count === 0">还没有渠道，用上方表单创建</UiEmpty>
        <UiEmpty v-else-if="channels.filtered.length === 0">
          没有匹配「{{ channels.filter.trim() }}」的渠道
        </UiEmpty>
        <template v-else>
          <div class="tw">
            <table>
              <thead>
                <tr>
                  <th>ID</th>
                  <th>名称</th>
                  <th>站型</th>
                  <th>地址</th>
                  <th>状态</th>
                  <th></th>
                </tr>
              </thead>
              <tbody>
                <template v-for="c in channels.filtered" :key="c.id">
                  <!-- data-ch-row 标出**数据行**。编辑/停用展开时会往 tbody 里插
                       一个 sub-row，而验收脚本按 `tbody tr` 数渠道数、按
                       `td:first-child` 取 id —— 不区分的话，展开着一行时数出来
                       会多一个，取 id 会取到那个 colspan 单元格里的表单文本。 -->
                  <tr :data-ch-row="c.id">
                    <td>
                      <code>{{ c.id }}</code>
                    </td>
                    <td :data-ch-name="c.id">{{ c.name }}</td>
                    <td>
                      <span class="badge">{{ c.site_family }}</span>
                    </td>
                    <td class="dim">{{ c.base_url }}</td>
                    <td :data-ch-status="c.id">
                      <span class="badge" :class="c.status === 'enabled' ? 'ok' : 'warn'">{{
                        c.status
                      }}</span>
                      <!-- 停用原因就显示在状态旁边：停用是要人来解除的，
                           看不到原因就解不了（FR-095） -->
                      <span
                        v-if="c.status === 'disabled' && c.disabled_reason !== undefined"
                        class="dim"
                        :data-ch-reason="c.id"
                      >
                        {{ c.disabled_reason }}
                      </span>
                    </td>
                    <td class="acts">
                      <button
                        class="btn outline sm"
                        :data-ch="c.id"
                        :data-name="c.name"
                        @click="open(c.id, c.name)"
                      >
                        查看
                      </button>
                      <button class="btn outline sm" :data-ch-edit="c.id" @click="startEdit(c)">
                        编辑
                      </button>
                      <button
                        v-if="c.status === 'enabled'"
                        class="btn danger sm"
                        :data-ch-disable="c.id"
                        @click="startDisable(c.id)"
                      >
                        停用
                      </button>
                      <button
                        v-else
                        class="btn outline sm"
                        :data-ch-enable="c.id"
                        :disabled="busy"
                        @click="enable(c.id)"
                      >
                        启用
                      </button>
                    </td>
                  </tr>
                  <!-- 编辑行。站型不给改：它由 Detect 判定，手改会让字段映射全错 -->
                  <tr v-if="editing === c.id" class="sub-row">
                    <td colspan="6">
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
                          <button class="btn outline sm" id="btn-ch-cancel" @click="cancel">
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
                  <!-- 停用行：原因必填（FR-095） -->
                  <tr v-if="disabling === c.id" class="sub-row">
                    <td colspan="6">
                      <div class="row-form">
                        <UiField label="停用原因（必填）" for="ch-dis-reason">
                          <input
                            id="ch-dis-reason"
                            v-model="disableReason"
                            placeholder="例：站点跑路 / 余额耗尽待充值"
                          />
                        </UiField>
                        <div class="endcap">
                          <button
                            class="btn danger sm"
                            id="btn-ch-disable-ok"
                            :disabled="busy"
                            @click="confirmDisable(c.id)"
                          >
                            确认停用
                          </button>
                          <button class="btn outline sm" id="btn-ch-disable-cancel" @click="cancel">
                            取消
                          </button>
                        </div>
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
  </div>
</template>
