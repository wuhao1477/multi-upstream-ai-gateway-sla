# 渠道详情、备份导入与中文展示实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让渠道详情成为渠道管理的二级路由，补齐 all-api-hub 导入后的账号缓存与上游 Key 自动登记，并统一前端中文枚举展示。

**Architecture:** 一级菜单只由 `web/src/router/panes.ts` 描述；详情使用 `/channels/:id` 路由参数，并通过路由元信息继承渠道菜单高亮。导入基础资源提交后，由生产 Runner 通过现有 Adapter 的 `Authenticate`/`Session` 读取 Key 列表，使用站型专属的 Key 明文解析器登记新 Key；Key 子流程失败只写入导入提醒。展示文案集中在 `web/src/utils/format.ts`，原始枚举仅用于判断和 `data-*` 属性。

**Tech Stack:** Go 1.x、pgx、Vue 3、Vue Router 5、Pinia、TypeScript、pnpm、现有 `httptest`/PostgreSQL 测试。

**Spec:** `docs/superpowers/specs/2026-09-11-channel-detail-import-localization-design.md`

## Global Constraints

- `PANES` 只包含一级菜单项；详情不可出现在左侧菜单。
- Key 自动登记必须发生在渠道/账号/凭证提交之后，Key 失败不得回滚基础资源。
- Key 明文不得进入导入响应、日志、错误文本或前端状态。
- NewAPI 使用 `POST /api/token/{id}/key`，Sub2API 使用 `GET /api/v1/keys/{id}`。
- 已有 Key 按 `(account_id, external_ref)` 去重；已有行可更新本次采集到的用量字段。
- 所有用户可见的站型、状态、结果、能力、支持级别和凭证类型必须使用中文映射。
- 不修改 `ai-gateway-architecture-review/`、`ai-gateway-uiux-optimization/` 等原型目录。
- 不运行破坏性 Git 命令；保留工作区中与本任务无关的已有变更。

---

### Task 1: 统一路由层级并移除详情一级菜单

**Files:**
- Modify: `web/src/router/panes.ts`
- Modify: `web/src/router/index.ts`
- Modify: `web/src/components/layout/AppSidebar.vue`
- Modify: `web/src/components/layout/AppTopbar.vue`
- Modify: `web/src/components/layout/NavIcon.vue`
- Modify: `web/src/views/ChannelsView.vue`
- Modify: `web/src/views/DetailView.vue`
- Modify: `web/src/components/res/AccountTable.vue`
- Modify: `web/src/components/res/KeyTable.vue`
- Test: `web/test/ui-contract.mjs`

**Interfaces:**
- Consumes: existing `Channel` store selection and `RouterView` shell.
- Produces: top-level `PaneName = 'channels' | 'accounts' | 'keys' | 'creds' | 'import'`; route name `channel-detail`; path `/channels/:id`; detail route meta `{ parent: 'channels', title: '渠道详情', note: '总览 / 账号 / Key / 分组 / 模型目录' }`.

- [ ] **Step 1: Write the failing route contract test**

```javascript
// web/test/ui-contract.mjs
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'

const panes = readFileSync(new URL('../src/router/panes.ts', import.meta.url), 'utf8')
const router = readFileSync(new URL('../src/router/index.ts', import.meta.url), 'utf8')
const sidebar = readFileSync(new URL('../src/components/layout/AppSidebar.vue', import.meta.url), 'utf8')

assert(!panes.includes("'detail'"), 'detail 不应再属于一级菜单枚举')
assert(router.includes("path: '/channels/:id'"), '详情必须使用渠道二级路径')
assert(router.includes("name: 'channel-detail'"), '详情必须使用独立路由名')
assert(sidebar.includes("route.meta.parent === p"), '详情路由必须高亮渠道管理')
```

- [ ] **Step 2: Run the contract test and verify it fails for the current implementation**

Run: `node web/test/ui-contract.mjs`  
Expected: FAIL because current `panes.ts` still contains `detail` and current router still uses `/detail`.

- [ ] **Step 3: Remove `detail` from the menu model and define the new detail route**

In `panes.ts`, remove `detail` from `PaneName`, `PANES`, and `PANE_ORDER`. In `router/index.ts`, keep `/` as the channel list, add the following route, and redirect the old entry:

```typescript
{
  path: '/channels/:id(\\d+)',
  name: 'channel-detail',
  component: DetailView,
  meta: {
    parent: 'channels',
    title: '渠道详情',
    note: '总览 / 账号 / Key / 分组 / 模型目录',
  },
},
{ path: '/detail', redirect: '/' },
```

- [ ] **Step 4: Make layout components consume the new route metadata**

Change `AppSidebar` active state to treat `route.name === p` or `route.meta.parent === p` as active. Change `AppTopbar` to read `title` and `note` from `route.meta`, with empty Chinese-safe defaults. Remove the `detail` prop branch from `NavIcon` and retain the existing channel icon for the channel menu item.

- [ ] **Step 5: Make all detail entry points navigate with the channel ID**

Replace the three old navigation calls with:

```typescript
await router.push({ name: 'channel-detail', params: { id: String(id) } })
```

`ChannelsView.open` must only navigate; `DetailView` becomes responsible for selecting the route channel. `AccountTable.openChannel` and `KeyTable.openChannel` use the same route.

- [ ] **Step 6: Bind `DetailView` selection to the route parameter**

Use `useRoute()` and watch `[() => channels.loaded, () => route.params.id]`. When the ID is a positive integer and the channel list is loaded, find the channel name and call `channels.select(id, name)`. Keep the existing view reset behavior on channel changes. Do not select a channel from `DetailView` based only on a stale Pinia ID.

- [ ] **Step 7: Run the route contract test and frontend type checks**

Run: `node web/test/ui-contract.mjs`  
Expected: PASS.  
Run: `cd web && pnpm run typecheck`  
Expected: PASS with no TypeScript errors.

---

### Task 2: Add safe session-based Key secret resolution

**Files:**
- Modify: `internal/collector/collector.go`
- Modify: `internal/collector/httpx.go`
- Modify: `internal/collector/newapi.go`
- Modify: `internal/collector/sub2api.go`
- Test: `internal/collector/key_secret_test.go`

**Interfaces:**
- Consumes: existing `Adapter`, `Session`, `FetchKeys`, and authenticated HTTP headers.
- Produces: `collector.KeySecretResolver` with `ResolveKeySecret(context.Context, Session, string) (string, error)`; `collector.KeyImportResult` with `Found`, `Imported`, `Skipped`, and `Failed` counts.

- [ ] **Step 1: Write failing resolver tests using real HTTP handlers**

```go
func TestNewAPIResolveKeySecretUsesPostRevealEndpoint(t *testing.T) {
    // Start an httptest server that returns {"data":{"key":"remote-secret"}}
    // only for POST /api/token/17/key and records the Authorization header.
    // Call NewNewAPIAdapter(...).ResolveKeySecret(..., "17") and assert the
    // method, path, bearer header, and returned secret.
}

func TestSub2APIResolveKeySecretReadsDetailKey(t *testing.T) {
    // Start an httptest server that returns {"data":{"key":"remote-secret"}}
    // only for GET /api/v1/keys/23. Assert the route and returned secret.
}

func TestResolveKeySecretRejectsMissingOrMaskedKey(t *testing.T) {
    // Return an empty key and then a masked key such as "sk-****abcd".
    // Both calls must return errors without including the key value in Error().
}
```

- [ ] **Step 2: Run the resolver tests and verify the expected missing-method failure**

Run: `go test ./internal/collector -run 'Test(NewAPIResolveKeySecret|Sub2APIResolveKeySecret|ResolveKeySecret)' -count=1`  
Expected: FAIL because `KeySecretResolver` and the resolver methods do not exist yet.

- [ ] **Step 3: Add the optional resolver contract and generic authenticated JSON request**

Add the interface and result type to `collector.go`. Refactor `Client.getJSONAuth` to delegate to a shared method that accepts the HTTP method, then add `postJSONAuth` for the NewAPI reveal endpoint. Preserve existing status handling and response-size limits.

- [ ] **Step 4: Implement NewAPI and Sub2API resolvers**

NewAPI must parse a positive numeric `keyRef`, call `postJSONAuth(ctx, session, "/api/token/"+keyRef+"/key")`, read `unwrapData(m)["key"]`, and reject empty or masked values. Sub2API must validate a non-empty numeric key reference, call `getJSONAuth(ctx, session, "/api/v1/keys/"+url.PathEscape(keyRef))`, read the detail `key`, and apply the same rejection rule. Never put the returned secret into an error string.

- [ ] **Step 5: Run resolver tests and existing adapter tests**

Run: `go test ./internal/collector -run 'Test(NewAPIResolveKeySecret|Sub2APIResolveKeySecret|ResolveKeySecret|NewAPI|Sub2API)' -count=1`  
Expected: PASS.

---

### Task 3: Import discovered Keys after the base resource transaction

**Files:**
- Modify: `internal/collection/runner.go`
- Modify: `internal/admin/config_api.go`
- Modify: `internal/admin/import_api.go`
- Modify: `internal/store/key_crud.go`
- Modify: `cmd/sla-core/main.go`
- Modify: `internal/collector/allapihub.go`
- Test: `internal/admin/import_tx_test.go`
- Test: `internal/collection/runner_test.go`

**Interfaces:**
- Consumes: `KeySecretResolver`, `store.KeyRefIndex`, `store.GroupIDByRef`, `store.UpdateKeyUsage`, `store.CreateKey`, and the existing `CredentialStore.ListByChannel`.
- Produces: `Server.ImportKeys func(context.Context, *pgx.Conn, channelID, accountID int64) (collector.KeyImportResult, error)`; `Runner.ImportKeys(context.Context, *pgx.Conn, store.Channel, []collector.Credential) (collector.KeyImportResult, error)`; `HubImportItem`/`HubImportResult` Key count fields.

- [ ] **Step 1: Write the failing import result and best-effort tests**

Add to `internal/admin/import_tx_test.go`:

```go
func TestKeyImportFailureDoesNotChangeImportedStatus(t *testing.T) {
    // Configure a Server.ImportKeys callback that returns an error.
    // Run the import-item application path and assert the item remains
    // imported, keys_failed is unchanged when no result exists, and warning
    // says the Key step failed without exposing any secret text.
}

func TestFinishImportAggregatesKeyCounts(t *testing.T) {
    // Build two HubImportItems with keys_found/keys_imported/keys_skipped/
    // keys_failed and assert HubImportResult totals are the sums.
}
```

Add to `internal/collection/runner_test.go`:

```go
func TestImportKeysSkipsExistingExternalRefAndCreatesNewKey(t *testing.T) {
    // Use the existing test database setup. Seed one existing external_ref,
    // expose one existing and one new key through an authenticated test
    // handler, and assert the result is Found=2, Skipped=1, Imported=1 with
    // exactly one new upstream_keys row.
}
```

- [ ] **Step 2: Run the new tests and verify they fail for the missing import path**

Run: `go test ./internal/admin ./internal/collection -run 'Test(KeyImportFailure|FinishImportAggregatesKeyCounts|ImportKeysSkipsExistingExternalRef)' -count=1`  
Expected: FAIL because the server callback, result fields, and Runner method do not exist.

- [ ] **Step 3: Extend import result types without exposing secrets**

Add `KeyImportResult` to the collector package. Add `KeysFound`, `KeysImported`, `KeysSkipped`, and `KeysFailed` JSON fields to `HubImportItem` and `HubImportResult`. Add an internal `AccountID int64` field with `json:"-"` to `HubImportItem` so the post-commit callback can target the exact imported account without putting that routing detail in the response.

- [ ] **Step 4: Record the exact account ID in every successful import path**

In `importOne`, set the internal account ID when creating a new account. In existing-channel and repair paths, resolve the account ID after the repair or before returning `skipped`. Keep the existing base transaction and commit behavior unchanged.

- [ ] **Step 5: Implement `Runner.ImportKeys` with duplicate-safe writes**

Add the method to `collection.Runner`. Resolve the channel adapter, select the passed credential, run the same optional `Authenticator.EnsureFresh` path as normal collection, call `Authenticate`, then `FetchKeys`. Get the existing account Key map through `store.KeyRefIndex`. For each discovered Key, increment `Found`; if its external reference already exists, update usage with `store.UpdateKeyUsage` and increment `Skipped`; otherwise resolve the secret through `KeySecretResolver`, resolve an existing group ID with `store.GroupIDByRef`, create the row with `store.CreateKey`, and increment `Imported`. Invalid references, secret resolution errors, and individual database errors increment `Failed` and continue. Return a top-level error only for channel/credential/session/list failures.

- [ ] **Step 6: Invoke the Key phase after `importOne` commits**

Add the optional callback to `admin.Server`. In the non-dry import loop, call it only after `importOne` returns without an error and the item has both channel and account IDs. Merge the returned counts into the item and append a Chinese warning when `Failed > 0`; when the callback returns an error, append a generic Chinese warning that does not include the error body or any secret. Never change an imported item to `failed` because the Key phase failed.

- [ ] **Step 7: Wire production credentials and Runner into the callback**

In `cmd/sla-core/main.go`, load the channel with `store.GetChannel`, load valid credentials with `CredentialStore.ListByChannel`, select the credential matching `accountID`, attach `store.QuotaPerUnit`, and call `runner.ImportKeys` with the existing connection. Set `srv.ImportKeys` to this closure. If no matching credential exists, return a non-secret error so the importer can show a warning.

- [ ] **Step 8: Aggregate counts and run the focused integration tests**

Extend `finishImport` to sum the four Key fields. Run: `SLA_TEST_DSN="$SLA_TEST_DSN" go test ./internal/admin ./internal/collection ./internal/collector ./internal/store -run 'Test(KeyImport|FinishImport|ImportKeys|ImportRepair|ImportRollsBack|NewAPIResolveKeySecret|Sub2APIResolveKeySecret)' -count=1`  
Expected: PASS when `SLA_TEST_DSN` is provided; tests that require the database retain their existing skip behavior when it is absent.

---

### Task 4: Refresh frontend stores and localize all visible enum values

**Files:**
- Modify: `web/src/views/ImportView.vue`
- Modify: `web/src/utils/format.ts`
- Modify: `web/src/utils/money.ts`
- Modify: `web/src/views/ChannelsView.vue`
- Modify: `web/src/views/AccountsView.vue`
- Modify: `web/src/views/KeysView.vue`
- Modify: `web/src/views/CredsView.vue`
- Modify: `web/src/views/DetailView.vue`
- Modify: `web/src/components/res/AccountTable.vue`
- Modify: `web/src/components/res/KeyTable.vue`
- Modify: `web/src/components/res/RegisterDrawers.vue`
- Modify: `web/src/api/types.ts`
- Test: `web/test/ui-contract.mjs`

**Interfaces:**
- Consumes: `HubImportResult` Key count fields and existing raw enum values.
- Produces: `familyLabel`, `statusLabel`, `importStatusLabel`, `capabilityLabel`, `supportLevelLabel`, `credentialTypeLabel`, and `anomalyLabel` display helpers; import completion reloads `channels`, `resources`, and `credentials` stores.

- [ ] **Step 1: Extend the frontend contract test with failing localization and refresh assertions**

```javascript
const format = readFileSync(new URL('../src/utils/format.ts', import.meta.url), 'utf8')
const importView = readFileSync(new URL('../src/views/ImportView.vue', import.meta.url), 'utf8')

assert(format.includes("skipped: '跳过'"), 'skip 必须显示为跳过')
assert(format.includes('function familyLabel'), '站型必须经过统一中文映射')
assert(importView.includes('res.reload()'), '导入后必须刷新账号与 Key store')
assert(importView.includes('creds.load()'), '导入后必须刷新凭证 store')
```

- [ ] **Step 2: Run the contract test and verify it fails**

Run: `node web/test/ui-contract.mjs`  
Expected: FAIL because the format helpers and store reload calls are not present.

- [ ] **Step 3: Add centralized Chinese label helpers**

In `web/src/utils/format.ts`, add explicit maps for `newapi`, `sub2api`, `unknown`; `account`, `keys`, `groups`, `pricing`, `model_catalog`; `supported`, `degraded`, `unsupported`; `ok`, `partial`, `failed`, `skipped`; `imported`, `would_import`, `detected`; `enabled`, `disabled`, `active`, `revoked`, `expired`, `insufficient_perm`, `valid`, and `invalid`; `newapi_access_token` and `sub2api_jwt`; and all anomaly kinds currently listed in `DetailView`. Unknown values must return Chinese phrases such as `未知站型`, `未知状态`, `未知能力`, or `未知异常`, never the raw English value.

- [ ] **Step 4: Replace visible raw enum interpolations**

Use the helpers in `ImportView`, `DetailView`, `ChannelsView`, `CredsView`, `AccountTable`, `KeyTable`, and `RegisterDrawers`. Keep raw values in comparisons and `data-*` attributes. Change `money.ts` fallbacks to Chinese and render rate limits as `次/分钟`; translate token billing unit labels to `令牌` wording.

- [ ] **Step 5: Refresh all stores after formal import and render Key counts**

Import `useResourcesStore` and `useCredentialsStore` in `ImportView`. After a non-dry import, run:

```typescript
await Promise.all([channels.load(), res.reload(), creds.load()])
```

Show the new aggregate Key counts in the result stats and include per-site Key counts in the explanation cell. Keep dry-run behavior free of authenticated Key calls.

- [ ] **Step 6: Run frontend checks**

Run: `node web/test/ui-contract.mjs`  
Expected: PASS.  
Run: `cd web && pnpm test`  
Expected: PASS.  
Run: `cd web && pnpm run build`  
Expected: PASS.

---

### Task 5: Full verification and delivery review

**Files:**
- Test: `verify/ui/verify-spa.mjs`
- Test: existing `verify/ui/verify-ui.mjs` and `verify/ui/verify-remote.mjs`

**Interfaces:**
- Consumes: completed router, import, resolver, store refresh, and label behavior.
- Produces: fresh evidence for route hierarchy, Key import behavior, localization, and no regression in the existing SPA.

- [ ] **Step 1: Add SPA checks for the new detail URL**

Extend `verify/ui/verify-spa.mjs` to open `/admin/ui/channels/1` after the channel fixture is available, assert `#pane-detail.on`, assert no `.nav-item[data-pane="detail"]`, and assert the channel menu item is active. Keep the existing `/admin/ui/creds` history-refresh check.

- [ ] **Step 2: Run Go verification**

Run: `go test ./... -count=1`  
Expected: exit code 0, with only the repository's existing environment-dependent skips.

- [ ] **Step 3: Run frontend verification**

Run: `cd web && pnpm test && pnpm run build`  
Expected: both commands exit 0.

- [ ] **Step 4: Run the UI verification when the local stack is available**

Run: `make test-ui`  
Expected: existing SPA and UI checks pass, including the new detail route and Chinese labels. If the stack is not configured, record the exact skipped prerequisite instead of claiming the UI check passed.

- [ ] **Step 5: Inspect the final diff and status**

Run: `git diff --check` and `git status --short`. Verify only the requested files plus the committed design/plan documents changed; do not stage unrelated existing files.

