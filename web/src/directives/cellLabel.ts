import type { Directive } from 'vue'

/**
 * `v-cell-label`：把表头文字抄进同列每个 `<td>` 的 `data-label`。
 *
 * 手机上表格**竖着摊开** —— 一行变成一张卡，每格左边印列名、右边印值
 * （见 `styles/app.css` 的移动端分支）。那个形态里 `<thead>` 整个隐藏，
 * 而 CSS 读不到另一个元素的文字，所以列名只能先落成属性，
 * 再由 `content: attr(data-label)` 取出来。
 *
 * 为什么是指令，而不是在每个 `<td>` 上手写 `data-label`：
 * 手写等于把表头文案抄第二遍，而抄的那一份不会跟着表头改 —— 改完列名之后，
 * 宽屏显示新名字、手机上还印着旧名字，**且两边都不报错**。本仓库已经有一条
 * 同类的成对约束（`th` 与 `td` 的 `data-col` 必须一一对应，见 KeyTable 的注释），
 * 那条靠注释提醒守住；这条交给代码守，就不用再靠人记。
 *
 * 为什么不等到窄屏再算：媒体查询的变化不经过 Vue。按当前屏宽决定算不算的话，
 * 转屏或拖窗口之后标签就是陈旧的 —— 而它看起来只是"这一格没有列名"，
 * 不像是个 bug。代价是宽屏也走一遍，所以只在值真的变了时才写 DOM。
 */
function label(table: HTMLTableElement): void {
  const head = table.tHead?.rows[0]
  if (head === undefined) return
  const names = [...head.cells].map((th) => th.textContent?.trim() ?? '')
  // 摊开那套样式挂在这个属性上，而属性只有走完上面这几行才落下 ——
  // 于是"能摊开"与"列名已就位"是同一件事。少了这层绑定的话，新加一张忘了写
  // `v-cell-label` 的表在手机上会摊成一列没有名字的值：布局看起来是对的，
  // 只是读不懂，而宽屏上完全正常。
  table.dataset.cellLabel = ''
  for (const body of table.tBodies) {
    for (const row of body.rows) {
      for (const cell of row.cells) {
        // 跨列的子行（行内编辑、二级展开）不属于任何一列 —— 给它安上第一列的
        // 名字比不安更糟：那张展开出来的表会顶着一个"渠道"的帽子。
        if (cell.colSpan > 1) continue
        const name = names[cell.cellIndex] ?? ''
        if (name !== '' && cell.dataset.label !== name) cell.dataset.label = name
      }
    }
  }
}

export const cellLabel: Directive<HTMLTableElement> = {
  mounted: label,
  updated: label,
}
