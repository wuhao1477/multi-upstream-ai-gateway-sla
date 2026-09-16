import { createPinia, setActivePinia } from 'pinia'
import type { CatalogResp } from '@/api/types'
import { useCatalogStore } from './catalog'

interface Deferred<T> {
  promise: Promise<T>
  resolve(value: T): void
}

function deferred<T>(): Deferred<T> {
  let resolve!: (value: T) => void
  const promise = new Promise<T>((r) => {
    resolve = r
  })
  return { promise, resolve }
}

function catalog(unit: string, modelName: string): CatalogResp {
  return {
    channel_id: 1,
    total: 1,
    limit: 50,
    offset: 0,
    unit,
    units: { usd: 1, token: 1 },
    items: [
      {
        model_name: modelName,
        billing_unit: unit,
        first_seen_at: '',
        last_seen_at: '',
        stale: false,
      },
    ],
  }
}

function assertEqual<T>(actual: T, expected: T, message: string): void {
  if (actual !== expected) throw new Error(`${message}: expected ${expected}, got ${actual}`)
}

async function testStaleCatalogCannotReplaceCurrentFilter(): Promise<void> {
  setActivePinia(createPinia())
  const all = deferred<CatalogResp>()
  const filtered = deferred<CatalogResp>()

  globalThis.slaAdminMock = {
    listSiteFamilies: async () => ({ count: 0, items: [] }),
    listChannels: async () => ({ count: 0, items: [] }),
    // 本用例不该碰全局目录。留成抛异常的桩而不是把类型改成可选：
    // 可选的话，漏 stub 一个真会被调到的函数只会在 store 的 catch 里被吞掉，
    // 用例照样绿。
    globalCatalog: async () => {
      throw new Error('unexpected globalCatalog')
    },
    createChannel: async (input) => {
      void input
      throw new Error('unexpected createChannel')
    },
    patchChannel: async (id, input) => {
      void id
      void input
      throw new Error('unexpected patchChannel')
    },
    channelInventory: async (id) => {
      void id
      throw new Error('unexpected channelInventory')
    },
    syncChannel: async (id) => {
      void id
      throw new Error('unexpected syncChannel')
    },
    channelCatalog: (_id, q) => (q.unit === 'usd' ? filtered.promise : all.promise),
  }

  const cat = useCatalogStore()
  const open = cat.open(1)
  cat.setUnit('usd')

  filtered.resolve(catalog('usd', 'new-filter-model'))
  await filtered.promise
  assertEqual(cat.unit, 'usd', 'filter state should change immediately')
  assertEqual(cat.items[0]?.model_name, 'new-filter-model', 'new filter response should render')

  all.resolve(catalog('', 'old-all-model'))
  await open
  assertEqual(cat.unit, 'usd', 'filter state should stay on the newer filter')
  assertEqual(cat.items[0]?.model_name, 'new-filter-model', 'stale catalog response should be ignored')
}

await testStaleCatalogCannotReplaceCurrentFilter()
