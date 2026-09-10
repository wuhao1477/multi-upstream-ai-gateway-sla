/**
 * 五个分栏的元信息。侧栏文案、顶栏标题与路由名共用一份定义 ——
 * 三处分别写死过一次，加一个分栏就得记得改三个地方。
 */
export type PaneName = 'channels' | 'detail' | 'register' | 'creds' | 'import'

export interface PaneMeta {
  /** 侧栏里的短名。 */
  nav: string
  /** 顶栏标题。 */
  title: string
  /** 顶栏标题右侧的小字，写这个分栏最容易被误解的那一点。 */
  note: string
}

export const PANES: Record<PaneName, PaneMeta> = {
  channels: { nav: '渠道商', title: '渠道商', note: '创建、探测站型、查看台账' },
  detail: { nav: '渠道详情', title: '渠道详情', note: '资产总览 / 采集 / 分组 / 目录 / Key' },
  register: { nav: '账号与 Key', title: '账号与 Key', note: '明文只进不出（FR-094）' },
  creds: { nav: '采集凭证', title: '采集凭证', note: '没有凭证就无法采集' },
  import: { nav: '批量导入', title: '批量导入', note: '以本方 Detect 为准，不信任导出声明' },
}

/** 侧栏顺序。 */
export const PANE_ORDER: PaneName[] = ['channels', 'detail', 'register', 'creds', 'import']
