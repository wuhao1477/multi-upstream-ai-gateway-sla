import { createServer } from 'vite'

const adminMock = `
const mock = () => {
  const m = globalThis.slaAdminMock
  if (m === undefined) throw new Error('admin mock not set')
  return m
}

export function listSiteFamilies() {
  return mock().listSiteFamilies()
}

export function listChannels() {
  return mock().listChannels()
}

export function createChannel(input) {
  return mock().createChannel(input)
}

export function patchChannel(id, input) {
  return mock().patchChannel(id, input)
}

export function channelInventory(id) {
  return mock().channelInventory(id)
}

export function syncChannel(id) {
  return mock().syncChannel(id)
}

export function channelCatalog(id, q) {
  return mock().channelCatalog(id, q)
}
`

const server = await createServer({
  root: import.meta.dirname + '/..',
  server: { middlewareMode: true },
  plugins: [
    {
      name: 'sla:test-admin-mock',
      enforce: 'pre',
      resolveId(id) {
        if (id === 'virtual:sla-test-admin-mock') return '\0sla-test-admin-mock'
        return undefined
      },
      load(id) {
        return id === '\0sla-test-admin-mock' ? adminMock : undefined
      },
      transform(code, id) {
        if (id.endsWith('/src/stores/channels.ts') || id.endsWith('/src/stores/catalog.ts')) {
          return code.replace("from '@/api/admin'", "from 'virtual:sla-test-admin-mock'")
        }
        return undefined
      },
    },
  ],
})

try {
  await server.ssrLoadModule('/src/stores/channels.test.ts')
  await server.ssrLoadModule('/src/stores/catalog.test.ts')
  // money 是纯函数、不碰 api/admin，所以不需要上面那套 mock 转写；
  // 挂在这儿只是因为 `pnpm test` 就指向本文件，不值得为它再起第二个 runner。
  await server.ssrLoadModule('/src/utils/money.test.ts')
} finally {
  await server.close()
}
