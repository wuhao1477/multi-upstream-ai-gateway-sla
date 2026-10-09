// 首次绘制前确定主题；与 stores/theme.ts 使用相同的偏好键和判断。
try {
  var t = localStorage.getItem('theme') || 'system'
  var dark =
    t === 'dark' ||
    (t === 'system' && window.matchMedia('(prefers-color-scheme: dark)').matches)
  document.documentElement.classList.toggle('dark', dark)
} catch (e) {
  // 浏览器禁止访问 localStorage 时保留默认浅色。
}
