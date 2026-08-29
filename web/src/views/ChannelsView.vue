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
          <select id="ch-family" v-model="family">
            <option value="">自动探测（推荐）</option>
            <option value="newapi">NewAPI 系</option>
            <option value="sub2api">Sub2API 系</option>
            <option value="asxs">ASXS（闭源）</option>
            <option value="unknown">未知</option>
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
                <tr v-for="c in channels.filtered" :key="c.id">
                  <td>
                    <code>{{ c.id }}</code>
                  </td>
                  <td>{{ c.name }}</td>
                  <td>
                    <span class="badge">{{ c.site_family }}</span>
                  </td>
                  <td class="dim">{{ c.base_url }}</td>
                  <td>
                    <span class="badge" :class="c.status === 'enabled' ? 'ok' : 'warn'">{{
                      c.status
                    }}</span>
                  </td>
                  <td>
                    <button
                      class="btn outline sm"
                      :data-ch="c.id"
                      :data-name="c.name"
                      @click="open(c.id, c.name)"
                    >
                      查看
                    </button>
                  </td>
                </tr>
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
