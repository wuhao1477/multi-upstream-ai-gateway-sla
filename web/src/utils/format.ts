/** 时间戳格式化。缺值显式写"从未"，不留空格 —— 空白单元格分不清"没有"和"没渲染"。 */
export function fmtTime(t: string | undefined | null): string {
  if (t === undefined || t === null || t === '') return '从未'
  return new Date(t).toLocaleString('zh-CN')
}

/**
 * 计价口径的展示文案。
 *
 * 口径必须与价格同格显示：per_call 是每次调用的绝对美元价，
 * per_1m_token 是倍率，两者数值区间重叠，光看数字无法分辨。
 * 未声明时**不假定默认值** —— 返回 null，由组件渲染成带 title 的告警文案。
 */
export function unitLabel(u: string | undefined | null): string | null {
  switch (u) {
    case 'per_call':
      return '/次'
    case 'per_1m_token':
      return '×倍率'
    case 'per_1k_token':
      return '×倍率(1k)'
    case 'per_token':
      return '×倍率(token)'
    default:
      return null
  }
}

/** 分段筛选按钮的文案。unknown 是 billing_unit IS NULL 的存量行。 */
export function unitChip(u: string): string {
  if (u === 'unknown') return '口径未知'
  return unitLabel(u) ?? '口径未知'
}
