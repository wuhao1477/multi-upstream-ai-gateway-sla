import { fileURLToPath, URL } from 'node:url'
import { mkdirSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'
import { defineConfig, type Plugin } from 'vite'
import vue from '@vitejs/plugin-vue'

/** 产物目录：直接落到 Go 包内，由 internal/admin/embed 用 go:embed 打进二进制。 */
const OUT_DIR = fileURLToPath(new URL('../internal/admin/webdist', import.meta.url))

/**
 * keepEmbedPlaceholder 在每次构建后把占位 .gitignore 写回产物目录。
 *
 * 为什么必须有：go:embed 的模式**匹配不到任何文件就是编译错误**，所以刚 clone
 * 下来（还没 npm run build）的仓库必须在 webdist/ 里已经有一个被 git 跟踪的文件，
 * 否则 go build ./... 直接失败。而 Vite 的 emptyOutDir 清目录时只跳过 .git，
 * .gitignore 会被一起删掉 —— 于是每次构建后都要补回来。
 *
 * 用 .gitignore 兼任占位与忽略规则（`*` + `!.gitignore`：目录进版本库、内容不进），
 * 配合 //go:embed all:webdist（all: 前缀才收下点号开头的文件）。
 * 对应地，HTTP 处理器必须拒绝点号开头的路径，别把这个文件当静态资源发出去。
 */
function keepEmbedPlaceholder(): Plugin {
  return {
    name: 'sla:keep-embed-placeholder',
    apply: 'build',
    closeBundle() {
      mkdirSync(OUT_DIR, { recursive: true })
      writeFileSync(join(OUT_DIR, '.gitignore'), '*\n!.gitignore\n')
    },
  }
}

export default defineConfig({
  plugins: [vue(), keepEmbedPlaceholder()],
  // 与后端路由一致：页面挂在 /admin/ui/，资源引用必须带这个前缀，
  // 否则 /assets/xxx.js 会打到 Caddy 代理的业务前缀上去。
  base: '/admin/ui/',
  resolve: {
    alias: { '@': fileURLToPath(new URL('./src', import.meta.url)) },
  },
  build: {
    outDir: OUT_DIR,
    emptyOutDir: true,
    target: 'es2022',
    // 不做代码分割：ESM 的 <script type="module"> 是 defer 语义，
    // 而 DOMContentLoaded 要等 defer 脚本抓取+执行完 —— 单块产物才能保证
    // Puppeteer 的 waitUntil:'domcontentloaded' 返回时 SPA 已经挂载完成。
    // 验收脚本大量依赖"页面加载完即可查选择器"，分包会让它变成竞态。
    //
    // 用 rolldown 的 codeSplitting:false 而不是已废弃的 inlineDynamicImports：
    // 后者在 Vite 8 会打一条 deprecation 警告，且语义只覆盖动态 import。
    rollupOptions: { output: { codeSplitting: false } },
  },
  server: {
    proxy: {
      // 开发态把 /admin/* 转给后端，但 /admin/ui 留给 Vite 自己（那是本页面）。
      '^/admin(?!/ui)': {
        target: process.env['SLA_DEV_BACKEND'] ?? 'http://127.0.0.1:8080',
        changeOrigin: true,
      },
    },
  },
})
