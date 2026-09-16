# 供应商图标（第三方资源，随仓库分发）

来源：[`@lobehub/icons-static-svg`](https://github.com/lobehub/lobe-icons)（MIT）。
文件名与该包 `icons/` 下的一致，未做任何改动 —— 只挑了实际用得到的那些。

**为什么把它们收进仓库而不是走 CDN**：`/admin/ui` 由 `go:embed` 打进二进制，
`internal/admin/web.go` 明确承诺「内网/离线环境也不依赖任何外链或第二个容器」。
走 CDN 会让这个界面在内网变成一排裂图，那是我们说过不会发生的事。

**这批是怎么挑的**：2026-09-15 实测一个真实 NewAPI 站点的 `/api/pricing`，
顶层 `vendors` 给出 35 家发行方、27 个不同的 `icon` 名（同一个图标被多家复用，
例如 Alibaba / 阿里巴巴 / Alibaba (China) 都是 `Qwen.Color`）。这里就是那 27 个。

**icon 名 → 文件名**：见 `web/src/utils/vendorIcon.ts`。规则是 PascalCase 转
kebab-case，三处例外（`DeepSeek.Color` / `XiaomiMiMo` / `Grok.Color`）在那里显式
列出 —— 实测这三个对不上那条规则，而错了只会静默退回字母块。前两个是文件名拼法
不同，第三个是**上游声明了一个根本不存在的彩色版**（lobehub 只有单色 `grok.svg`）。

第 28 个文件 `grok.svg` 是 2026-09-15 跑验收时，第二个真站点报缺才补的 ——
上面那 27 个只覆盖了第一个站。这条路径现在有断言守着（verify-ui.mjs 的
「发行方图标」一条会把缺的名字直接打出来）。

**单色图标**（`fill="currentColor"`，11 个）走 `<img>` 时解析不到宿主页面的颜色，
一律渲染成黑色。所以 `.vmark.img` 垫了白底——否则深色主题下它们是看不见的。

**没收进来的 vendor 怎么办**：退回名字取色的字母块。这是刻意的 —— 站点随时会
加新 vendor，而"缺图标"不该变成"这一行渲染不出来"。要补就往这个目录再放一个
同名文件，不需要改代码。
