import type { CatalogQuery } from '@/api/admin'
import type {
  CatalogResp,
  GlobalCatalogResp,
  InventoryResp,
  ListResp,
  SiteFamilyInfo,
  SyncResult,
} from '@/api/types'

interface StoreTestAdminMock {
  listSiteFamilies(): Promise<ListResp<SiteFamilyInfo>>
  listChannels(): Promise<{ count: number; items: [] }>
  createChannel(input: unknown): Promise<unknown>
  patchChannel(id: number, input: unknown): Promise<unknown>
  channelInventory(id: number): Promise<InventoryResp>
  syncChannel(id: number): Promise<SyncResult>
  channelCatalog(id: number, q: CatalogQuery): Promise<CatalogResp>
  globalCatalog(q: CatalogQuery): Promise<GlobalCatalogResp>
}

declare global {
  var slaAdminMock: StoreTestAdminMock | undefined
}
