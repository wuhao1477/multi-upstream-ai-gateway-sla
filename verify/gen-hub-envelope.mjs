#!/usr/bin/env node
/**
 * 重新生成 `verify/fixtures/all-api-hub-encrypted-backup.json`。
 *
 * **它 import 的是 all-api-hub 自己的加密实现**，不是我方重写一份加密器 ——
 * 后者只能证明"我解得开我自己加的密"，两边一起错也照样绿（CLAUDE.md §1：
 * mock 是照实现者的理解写出来的，所以它永远不会推翻那个理解）。
 *
 * 用法：
 *
 *   git clone --depth 1 https://github.com/qixing-jk/all-api-hub /tmp/aah
 *   node verify/gen-hub-envelope.mjs /tmp/aah > verify/fixtures/all-api-hub-encrypted-backup.json
 *
 * 平时不用跑：信封格式是 v1、常量写死在上游源码里，夹具不会自己过期。
 * 要跑的时机只有一个 —— 上游把 `WEBDAV_BACKUP_ENCRYPTION` 的 v 抬上去了。
 *
 * 明文是**合成的**小备份：被测对象是信封格式与 AES-GCM 解密，不是备份内容，
 * 而拿真备份来加密等于把真凭证以密文形式写进仓库（密码还就在旁边）。
 */
import { execFileSync } from 'node:child_process'
import path from 'node:path'

const repo = process.argv[2]
if (repo === undefined) {
  console.error('用法: node verify/gen-hub-envelope.mjs <all-api-hub 仓库路径>')
  process.exit(2)
}

const src = path.join(repo, 'src/services/webdav/webdavBackupEncryption.ts')
const { encryptWebdavBackupContent } = await import(src)

const password = 'correct horse battery staple'
const plaintext = JSON.stringify({
  version: '4.0',
  timestamp: 1757000000000,
  accounts: {
    accounts: [
      {
        id: 'a1',
        site_name: '演示站',
        site_url: 'https://demo.example',
        site_type: 'NewAPI',
      },
    ],
  },
})

const envelope = await encryptWebdavBackupContent({ content: plaintext, password })

let commit = 'unknown'
try {
  commit = execFileSync('git', ['-C', repo, 'rev-parse', 'HEAD']).toString().trim()
} catch {
  // 不是 git 工作树也无所谓，provenance 里写 unknown 比中断好
}

console.log(
  JSON.stringify(
    {
      _provenance:
        `由 all-api-hub 自己的 encryptWebdavBackupContent 产出；` +
        `上游 qixing-jk/all-api-hub commit ${commit}；生成于 ${new Date().toISOString().slice(0, 10)}。` +
        `重新生成见 verify/gen-hub-envelope.mjs。`,
      password,
      plaintext,
      envelope,
    },
    null,
    2,
  ),
)
