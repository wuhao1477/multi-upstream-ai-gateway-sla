import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'

const panes = readFileSync(new URL('../src/router/panes.ts', import.meta.url), 'utf8')
const router = readFileSync(new URL('../src/router/index.ts', import.meta.url), 'utf8')
const sidebar = readFileSync(new URL('../src/components/layout/AppSidebar.vue', import.meta.url), 'utf8')
const format = readFileSync(new URL('../src/utils/format.ts', import.meta.url), 'utf8')
const importView = readFileSync(new URL('../src/views/ImportView.vue', import.meta.url), 'utf8')

assert(!panes.includes("'detail'"), 'detail 不应再属于一级菜单枚举')
assert(router.includes("path: '/channels/:id"), '详情必须使用渠道二级路径')
assert(router.includes("name: 'channel-detail'"), '详情必须使用独立路由名')
assert(sidebar.includes('route.meta.parent === p'), '详情路由必须高亮渠道管理')
assert(format.includes("skipped: '跳过'"), 'skip 必须显示为跳过')
assert(format.includes('function familyLabel'), '站型必须经过统一中文映射')
assert(importView.includes('res.reload()'), '导入后必须刷新账号与 Key store')
assert(importView.includes('creds.load()'), '导入后必须刷新凭证 store')
