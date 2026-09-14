import { createPinia, setActivePinia } from 'pinia'
import type { InventoryResp, SyncResult } from '@/api/types'
import { useChannelsStore } from './channels'

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

function inventory(channelID: number): InventoryResp {
  return {
    channel_id: channelID,
    name: `channel-${channelID}`,
    site_family: 'newapi',
    accounts: channelID,
    keys: 0,
    groups: 0,
    catalog_models: 0,
    quota_total_usd: 0,
    anomalies: [],
  }
}

function assertEqual<T>(actual: T, expected: T, message: string): void {
  if (actual !== expected) throw new Error(`${message}: expected ${expected}, got ${actual}`)
}

async function testStaleInventoryCannotReplaceSelectedChannel(): Promise<void> {
  setActivePinia(createPinia())
  const first = deferred<InventoryResp>()
  const second = deferred<InventoryResp>()

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
    channelInventory: (id) => (id === 1 ? first.promise : second.promise),
    syncChannel: async (id) => {
      void id
      throw new Error('unexpected syncChannel')
    },
    channelCatalog: async (id, q) => {
      void id
      void q
      throw new Error('unexpected channelCatalog')
    },
  }

  const channels = useChannelsStore()
  const selectFirst = channels.select(1, 'old')
  const selectSecond = channels.select(2, 'new')

  second.resolve(inventory(2))
  await selectSecond
  assertEqual(channels.inventory?.channel_id, 2, 'newer selection should load its inventory')

  first.resolve(inventory(1))
  await selectFirst
  assertEqual(channels.currentID, 2, 'selected channel should stay on the newer selection')
  assertEqual(channels.inventory?.channel_id, 2, 'stale inventory response should be ignored')
}

async function testStaleSyncCannotReplaceSelectedChannel(): Promise<void> {
  setActivePinia(createPinia())
  const firstSync = deferred<SyncResult>()

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
    channelInventory: async (id) => inventory(id),
    syncChannel: (id) => {
      if (id !== 1) throw new Error(`unexpected syncChannel ${id}`)
      return firstSync.promise
    },
    channelCatalog: async (id, q) => {
      void id
      void q
      throw new Error('unexpected channelCatalog')
    },
  }

  const channels = useChannelsStore()
  await channels.select(1, 'old')
  const syncOld = channels.sync()
  await channels.select(2, 'new')

  firstSync.resolve({
    channel_id: 1,
    site_family: 'newapi',
    started_at: '',
    elapsed_ms: 1,
    items: [],
  })
  await syncOld

  assertEqual(channels.currentID, 2, 'selected channel should stay on the newer selection')
  assertEqual(channels.syncResult, null, 'stale sync response should be ignored')
}

await testStaleInventoryCannotReplaceSelectedChannel()
await testStaleSyncCannotReplaceSelectedChannel()
