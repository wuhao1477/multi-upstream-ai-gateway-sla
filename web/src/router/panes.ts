/**
 * 分栏的元信息。侧栏文案、顶栏标题与路由名共用一份定义 ——
 * 三处分别写死过一次，加一个分栏就得记得改三个地方。
 *
 * 分组反映的是**四种对象、四种视角**（P1 的实体关系见 02 数据模型）：
 *   渠道 = 从哪接入 · 账号 = 钱归谁 · Key = 用哪把凭证 · 分组/目录 = 能调什么
 * 「账号」指的是**上游站点上的账号**，不是登录本网关的人 —— P1 没有平台
 * 用户实体（gateway_clients 已在 022 迁移里 DROP），那属于 P2 的 FR-120。
 *
 * 「采集凭证」曾经是第五个分栏，2026-09-13 并进账号页：库里
 * `collector_credentials` 有 UNIQUE(account_id)（023 迁移），一个账号最多
 * 一条 —— 它是账号的属性，不是第五种对象，摆成分栏等于在界面上多编了一个
 * 实体出来。凭证的登记与状态现在都在账号行上。
 */
export type PaneName = 'channels' | 'accounts' | 'keys' | 'models' | 'import'

/**
 * 顶层导航。**两项是有意的**：一边是"管上游资产"（控制台），另一边是
 * "查这个模型哪些渠道有"（模型目录）。
 *
 * 为什么模型目录不再待在侧栏里：它与侧栏那四项不是同一类任务 ——
 * 那四项都在**管**某种对象（渠道 / 账号 / Key / 导入），有共同的侧栏上下文；
 * 模型目录是**查**，而且它自己就有一整列筛选。塞进侧栏的结果是两列筛选
 * 并排（侧栏 + 它自己的左栏），屏幕被切成三段。
 */
export type TopNav = 'console' | 'models'

export const TOP_NAV: { key: TopNav; label: string; route: PaneName; note: string }[] = [
  { key: 'console', label: '控制台', route: 'channels', note: '管上游资产：渠道 / 账号 / Key / 导入' },
  { key: 'models', label: '模型目录', route: 'models', note: '查这个模型哪些渠道有' },
]

export interface PaneMeta {
  /** 侧栏里的短名。 */
  nav: string
  /** 顶栏标题。 */
  title: string
  /** 顶栏标题右侧的小字，写这个分栏最容易被误解的那一点。 */
  note: string
  /** 侧栏分区标题。同一分区的连续项共用一个。 */
  section: string
  /** 归属哪个顶层导航。顶栏据此高亮，App 据此决定要不要渲染侧栏。 */
  top: TopNav
}

export const PANES: Record<PaneName, PaneMeta> = {
  channels: {
    nav: '渠道管理',
    title: '渠道管理',
    note: '接入与运行状态；资产概览可二级展开',
    section: '上游资源',
    top: 'console',
  },
  accounts: {
    nav: '账号管理',
    title: '上游账号',
    note: '余额与采集凭证都归属在账号上，不是平台登录用户',
    section: '上游资源',
    top: 'console',
  },
  keys: {
    nav: 'Key 管理',
    title: '上游 Key',
    note: '明文只进不出（FR-094）；配额是约束不是钱',
    section: '上游资源',
    top: 'console',
  },
  models: {
    nav: '模型目录',
    title: '模型目录',
    note: '上游声明有什么，不是"已登记可路由"；一行一个模型，展开看哪些渠道有',
    section: '上游资源',
    top: 'models',
  },
  import: {
    nav: '批量导入',
    title: '批量导入',
    note: '以本方 Detect 为准，不信任导出声明',
    section: '运维',
    top: 'console',
  },
}

/**
 * 侧栏顺序。**只有控制台那一侧的分栏**——模型目录是顶层导航的另一半，
 * 不在侧栏里（它自己就有一整列筛选，再套一层侧栏等于把屏幕切成三段）。
 */
export const PANE_ORDER: PaneName[] = ['channels', 'accounts', 'keys', 'import']
