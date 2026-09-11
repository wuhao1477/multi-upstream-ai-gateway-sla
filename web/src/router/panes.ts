/**
 * 分栏的元信息。侧栏文案、顶栏标题与路由名共用一份定义 ——
 * 三处分别写死过一次，加一个分栏就得记得改三个地方。
 *
 * 分组反映的是**四种对象、四种视角**（P1 的实体关系见 02 数据模型）：
 *   渠道 = 从哪接入 · 账号 = 钱归谁 · Key = 用哪把凭证 · 分组/目录 = 能调什么
 * 「账号」指的是**上游站点上的账号**，不是登录本网关的人 —— P1 没有平台
 * 用户实体（gateway_clients 已在 022 迁移里 DROP），那属于 P2 的 FR-120。
 */
export type PaneName = 'channels' | 'detail' | 'accounts' | 'keys' | 'creds' | 'import'

export interface PaneMeta {
  /** 侧栏里的短名。 */
  nav: string
  /** 顶栏标题。 */
  title: string
  /** 顶栏标题右侧的小字，写这个分栏最容易被误解的那一点。 */
  note: string
  /** 侧栏分区标题。同一分区的连续项共用一个。 */
  section: string
}

export const PANES: Record<PaneName, PaneMeta> = {
  channels: {
    nav: '渠道管理',
    title: '渠道管理',
    note: '接入与运行状态；资产概览可二级展开',
    section: '上游资源',
  },
  detail: {
    nav: '渠道详情',
    title: '渠道详情',
    note: '总览 / 账号 / Key / 分组 / 模型目录',
    section: '上游资源',
  },
  accounts: {
    nav: '账号管理',
    title: '上游账号',
    note: '账号余额的归属方，不是平台登录用户',
    section: '上游资源',
  },
  keys: {
    nav: 'Key 管理',
    title: '上游 Key',
    note: '明文只进不出（FR-094）；配额是约束不是钱',
    section: '上游资源',
  },
  creds: {
    nav: '采集凭证',
    title: '采集凭证',
    note: '没有凭证就无法采集',
    section: '运维',
  },
  import: {
    nav: '批量导入',
    title: '批量导入',
    note: '以本方 Detect 为准，不信任导出声明',
    section: '运维',
  },
}

/** 侧栏顺序。 */
export const PANE_ORDER: PaneName[] = [
  'channels',
  'detail',
  'accounts',
  'keys',
  'creds',
  'import',
]
