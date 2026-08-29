import { defineStore } from 'pinia'
import { computed, ref } from 'vue'
import * as adminApi from '@/api/admin'
import type { CredentialItem } from '@/api/types'
import { useToastStore } from './toast'

export const useCredentialsStore = defineStore('credentials', () => {
  const toast = useToastStore()

  const list = ref<CredentialItem[]>([])
  const loaded = ref(false)
  const count = computed(() => list.value.length)

  /**
   * 拉凭证列表。失败**静默**：启动时会自动调一次，此时可能还没填令牌，
   * 弹一个"加载失败"只会挡住真正该看的提示（旧版同此行为）。
   */
  async function load(): Promise<void> {
    try {
      const d = await adminApi.listCredentials()
      list.value = d.items
      loaded.value = true
    } catch {
      /* 未配置或未填令牌时静默 */
    }
  }

  async function save(input: adminApi.SaveCredentialInput): Promise<boolean> {
    try {
      const d = await adminApi.saveCredential(input)
      // 后端回 cred_type + note，两者都要展示：cred_type 是判定结果
      // （运维据此确认"填的字段被认成了哪种凭证"），note 是续期方式提示。
      const o = (d ?? {}) as { cred_type?: string; note?: string }
      const kind = o.cred_type ?? '未知类型'
      toast.show(`凭证已登记（${kind}）\n${o.note ?? ''}`, 'ok')
      await load()
      return true
    } catch (e) {
      toast.fail('登记凭证失败', e)
      return false
    }
  }

  return { list, loaded, count, load, save }
})
