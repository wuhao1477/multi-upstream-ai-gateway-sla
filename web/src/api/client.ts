/**
 * 带令牌的 fetch 薄封装。
 *
 * 三件事值得说明：
 *  1. 令牌只存 localStorage，绝不落服务端（web.go 的注释与 06 §1 的边界一致）。
 *  2. 空令牌**在客户端就拦下**，不发请求 —— 否则每次都换回一个 401，
 *     用户看到的是"接口报错"而不是"你还没填令牌"。
 *  3. 失败时抛 ApiError 且带上 status 与已解析的响应体：同步接口用
 *     429/409/422/502 区分限流、前置条件不满足、认证失败，界面要按状态码分别措辞。
 *  4. 令牌失效（401/403）与没填令牌走**同一个出口** onUnauthorized —— 由
 *     main.ts 接到"清掉令牌 + 跳登录页"。放在这里而不是每个调用方各判一次：
 *     漏掉一处的症状是那一栏静静地空着，而别处都跳走了。
 */
import type { ErrorResp } from './types'

/** localStorage 的键名，与旧版页面保持一致（升级后用户不用重填令牌）。 */
const TOKEN_KEY = 'adminToken'

export function getToken(): string {
  try {
    return localStorage.getItem(TOKEN_KEY) ?? ''
  } catch {
    // 隐私模式下访问 localStorage 会抛异常，退化成"本次会话没有令牌"
    return ''
  }
}

export function setToken(v: string): void {
  try {
    localStorage.setItem(TOKEN_KEY, v)
  } catch {
    /* 同上，存不进去也不该让界面崩 */
  }
}

/**
 * 令牌失效时的回调，由 main.ts 注入。
 *
 * 不在这里 import router：一个 fetch 薄封装不该依赖路由，而且两边会互相 import。
 */
let onUnauthorized: (() => void) | undefined

export function setUnauthorizedHandler(f: () => void): void {
  onUnauthorized = f
}

/** 请求失败。status 为 HTTP 状态码，body 是已解析的响应体（可能为 undefined）。 */
export class ApiError extends Error {
  readonly status: number
  readonly body: unknown

  constructor(status: number, message: string, body?: unknown) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.body = body
  }
}

/**
 * 没填令牌就发起了请求。单独一个类型，方便界面提示"先填令牌"而不是"请求失败"。
 *
 * 文案里保留 ADMIN_TOKEN 这个字面量：它是环境变量名，运维照它去找配置项。
 */
export class MissingTokenError extends ApiError {
  constructor() {
    super(0, '请先填入 ADMIN_TOKEN')
    this.name = 'MissingTokenError'
  }
}

interface RequestOptions {
  // PUT 是给 /admin/hub-sync 那种「整份配置覆盖」用的；改字段的接口一律 PATCH。
  method?: 'GET' | 'POST' | 'PUT' | 'PATCH' | 'DELETE'
  /** 对象会被 JSON 序列化；字符串按原样发送（导入端点直接发文件内容）。 */
  body?: unknown
  /**
   * 不设默认超时是刻意的：同步接口服务端给 5 分钟、导入给 8 分钟，
   * 任何默认超时都会在正常流程里误杀。需要超时的调用方自己传 signal。
   */
  signal?: AbortSignal
}

function errMessage(body: unknown, fallback: string): string {
  if (typeof body === 'object' && body !== null && 'error' in body) {
    const e = (body as ErrorResp).error
    if (typeof e === 'string' && e !== '') return e
  }
  return fallback
}

/**
 * 发一个 /admin/* 请求并按 T 解析响应。
 *
 * 路径用绝对路径（如 '/admin/channels'）：页面挂在 /admin/ui/ 下，
 * 相对路径会拼成 /admin/ui/admin/channels。开发态由 Vite 代理转给后端。
 */
export async function api<T>(path: string, opts: RequestOptions = {}): Promise<T> {
  const token = getToken()
  if (token === '') {
    onUnauthorized?.()
    throw new MissingTokenError()
  }

  const headers: Record<string, string> = { Authorization: `Bearer ${token}` }
  let payload: string | undefined
  if (opts.body !== undefined) {
    payload = typeof opts.body === 'string' ? opts.body : JSON.stringify(opts.body)
    headers['Content-Type'] = 'application/json'
  }

  const init: RequestInit = { method: opts.method ?? 'GET', headers }
  if (payload !== undefined) init.body = payload
  if (opts.signal !== undefined) init.signal = opts.signal

  const resp = await fetch(path, init)
  // 401/403 在**解析响应体之前**判：反向代理插进来的 HTML 错误页会走下面那条
  // "非 JSON 就原文当消息"的早退分支，放在后面判就漏掉了这一路。
  //
  // 403 一并算：后端目前只回 401，但"这把令牌不让你干这件事"与"这把令牌不对"
  // 对界面是同一件事 —— 都得重新拿一把。
  if (resp.status === 401 || resp.status === 403) onUnauthorized?.()

  const text = await resp.text()
  let body: unknown
  if (text !== '') {
    try {
      body = JSON.parse(text)
    } catch {
      // 非 JSON 响应（如反向代理插进来的 HTML 错误页）——原文当消息用
      body = undefined
      if (!resp.ok) throw new ApiError(resp.status, text.slice(0, 200))
    }
  }

  if (!resp.ok) {
    throw new ApiError(resp.status, errMessage(body, `HTTP ${resp.status}`), body)
  }
  return body as T
}
