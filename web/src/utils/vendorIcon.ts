/**
 * 发行方图标：上游给的 icon 名 → 仓库里那份 SVG 的 URL。
 *
 * 上游 `/api/pricing` 的 `vendors[]` 里每项带 `icon`，实测取值是 lobehub 的图标
 * 名（`OpenAI` / `Claude.Color` / `Zhipu.Color`…）。我们把用得到的那些 SVG
 * **收进仓库**（`assets/vendor/`，见那里的 README）：`/admin/ui` 由 go:embed 打进
 * 二进制，`internal/admin/web.go` 承诺过内网/离线可用 —— 走 CDN 会让这个界面
 * 在内网变成一排裂图。
 *
 * 取不到图标时**退回字母块**而不是留空：站点随时会加新 vendor，
 * "缺图标"不该变成"这一行渲染不出来"。
 */

/**
 * 编译期收集 assets/vendor 下的全部 SVG。
 *
 * 用 glob 而不是逐个 import：补图标时只要往那个目录丢一个同名文件，
 * 不需要回来改代码（README 里也是这么写的，两处必须一致）。
 * `?url` 而不是 `?raw`：走 <img> 不走 v-html，既没有 XSS 面，
 * 又只在真正渲染到某个 vendor 时才发那一个请求。
 */
const FILES = import.meta.glob<string>('../assets/vendor/*.svg', {
  eager: true,
  query: '?url',
  import: 'default',
})

/** 文件名（不含扩展名）→ URL。 */
const BY_NAME: Record<string, string> = {}
for (const [path, url] of Object.entries(FILES)) {
  const base = path.split('/').pop()?.replace(/\.svg$/, '')
  if (base !== undefined) BY_NAME[base] = url
}

/**
 * icon 名不符合 PascalCase→kebab 规则的那几个，**实测得出**。
 *
 * 逐个列而不是想办法把规则改得更聪明：规则再聪明也只是在猜，而猜错是静默的
 * （退回字母块，看起来像"这家没图标"）。列在这里的每一行都是我拿名字去
 * 那个包里对过的。
 */
const ALIAS: Record<string, string> = {
  'DeepSeek.Color': 'deepseek-color', // 规则会算出 deep-seek-color，实测 404
  XiaomiMiMo: 'xiaomimimo', // 规则会算出 xiaomi-mi-mo，实测 404
  // lobehub 根本没有 grok 的彩色版（只有单色 grok.svg 与 grok-text.svg），
  // 而上游照样声明 Grok.Color。2026-09-15 验收在第二个真站点上撞到的。
  'Grok.Color': 'grok',
}

/** PascalCase / Dot.Case → kebab-case。`Claude.Color` → `claude-color`。 */
function kebab(s: string): string {
  return s
    .replace(/\./g, '-')
    .replace(/([a-z0-9])([A-Z])/g, '$1-$2')
    .replace(/([A-Z]+)([A-Z][a-z])/g, '$1-$2')
    .toLowerCase()
}

/** 取图标 URL；仓库里没有这个图标就返回 null，由调用方退回字母块。 */
export function vendorIconURL(icon: string | undefined | null): string | null {
  if (icon === undefined || icon === null || icon === '') return null
  const file = ALIAS[icon] ?? kebab(icon)
  return BY_NAME[file] ?? null
}

/**
 * 字母块的色相。同一个名字永远同一个颜色 —— 换一次颜色就等于换了一个
 * "看起来不一样的供应商"，而人是靠颜色在一排 chip 里认它的。
 */
export function vendorHue(name: string): number {
  let h = 0
  for (let i = 0; i < name.length; i++) h = (h * 31 + name.charCodeAt(i)) % 360
  return h
}

/** 字母块上那个字。取首个**码位**而不是 `name[0]` —— 中文与 emoji 都是多字节。 */
export function vendorInitial(name: string): string {
  return [...name][0] ?? '?'
}
