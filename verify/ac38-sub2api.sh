#!/usr/bin/env bash
# AC-38 的 **sub2api 一族**：对一个真实 Sub2API 站点跑一次手动刷新并逐条断言。
#
# 为什么要单独一个脚本，而不是塞进 ui-stack.sh 的 58 项：
#   那 58 项由 `verify/pick-upstream.mjs` 选站，而它**刻意只认 NewAPI 系**
#   （每个探测都是 NewAPI 专属的，理由写在那个文件第 35–42 行）。
#   AC-38 的判定基准是 `collector.All()` 的**每一族**都跑过一次，而 sub2api
#   那族此前在 HEAD 上没有可跑的站 —— 退出标准①因此按 14 §3
#   「不可判定即不通过」记为未通过（P1-release-readiness §1）。
#
# ⚠️ 站点**从 HUB_FILE 探活选取，绝不写死**（CLAUDE.md §1 + 记忆「靶子别写死」）：
#   Sub2API 的 access_token 是短寿 JWT。2026-09-01 那份导出里 26 个 sub2api
#   只有 1 个未过期，其余过期 65～3998 小时 —— 写死站名换来的是明天必红，
#   且红的理由与被测代码无关。选法：遍历 site_type=sub2api 的条目，
#   先确认公开指纹与无验证盾，再确认 /api/v1/auth/me 返回真实 JSON 账号；
#   不能把 SPA 的 200 HTML 回退页误当成可采集站点。
#
# ⚠️ 库用**临时 PG**，不碰内网真库：AC-38 的验证环境是 REAL（14 §0），
#   REAL 要求的是真**站点**，不是真库。而库里没有 DELETE 渠道的入口，
#   往真库写一行只为跑一次验收，是不可逆的代价。
#
# 用法：HUB_FILE=~/Downloads/all-api-hub-backup-*.json verify/ac38-sub2api.sh
# 显式授权站点：AC38_BASE_URL=https://... AC38_ACCESS_TOKEN_FILE=/private/token-file verify/ac38-sub2api.sh
# 第二种模式只用于人工已取得凭证的验收；不会放宽批量导入对 Turnstile 的规则。
set -euo pipefail
cd "$(dirname "$0")/.."
umask 077

HUB_FILE="${HUB_FILE:-}"
AC38_BASE_URL="${AC38_BASE_URL:-}"
AC38_ACCESS_TOKEN_FILE="${AC38_ACCESS_TOKEN_FILE:-}"
if [ -n "$AC38_BASE_URL$AC38_ACCESS_TOKEN_FILE" ]; then
  [ -n "$AC38_BASE_URL" ] && [ -n "$AC38_ACCESS_TOKEN_FILE" ] && [ -f "$AC38_ACCESS_TOKEN_FILE" ] || {
    echo "显式站点模式必须同时给 AC38_BASE_URL 与 AC38_ACCESS_TOKEN_FILE"
    exit 2; }
elif ! { [ -n "$HUB_FILE" ] && [ -f "$HUB_FILE" ]; }; then
  echo "需要 HUB_FILE 指向 all-api-hub 导出 JSON（真上游来源，CLAUDE.md §1 禁止 mock）"
  exit 2
fi

CT=ac38pg
PORT=18435       # 184xx 段：554xx 在临时端口段里会被出站连接借走（见 test-migrate.sh 顶部）
APIPORT=18082
DSN="postgres://postgres:x@127.0.0.1:${PORT}/sla"
TOKEN="ac38-sub2api-$$"

CORE_PID=""
AC38_TMP=""
cleanup() {
  if [ -n "$CORE_PID" ]; then
    kill "$CORE_PID" 2>/dev/null || true
    wait "$CORE_PID" 2>/dev/null || true
  fi
  docker rm -f -v "$CT" >/dev/null 2>&1 || true
  if [ -n "$AC38_TMP" ] && [ -d "$AC38_TMP" ]; then
    rm -rf "$AC38_TMP"
  fi
  return 0
}
trap cleanup EXIT
cleanup
AC38_TMP=$(mktemp -d "${TMPDIR:-/tmp}/ac38-sub2api.XXXXXX")
LOG="$AC38_TMP/core.log"
RESP_JSON="$AC38_TMP/resp.json"
SYNC_JSON="$AC38_TMP/sync.json"
CRED_JSON="$AC38_TMP/cred.json"
KEY_JSON="$AC38_TMP/key.json"
ACC_JSON="$AC38_TMP/acc.json"
KEYREQ_JSON="$AC38_TMP/keyreq.json"
TOKEN_FILE="$AC38_TMP/access-token"

echo "── 1/6 探活选一个真 Sub2API 站（不写死）──"
PICK=$(HUB_FILE="$HUB_FILE" TOKEN_FILE="$TOKEN_FILE" AC38_BASE_URL="$AC38_BASE_URL" \
  AC38_ACCESS_TOKEN_FILE="$AC38_ACCESS_TOKEN_FILE" python3 - <<'PY'
import base64, json, os, sys, time, urllib.error, urllib.request
explicit_base = os.environ.get("AC38_BASE_URL", "").rstrip("/")
if explicit_base:
    token_file = os.environ["AC38_ACCESS_TOKEN_FILE"]
    token = open(token_file).read().strip()
    if not token:
        print("显式站点令牌文件为空", file=sys.stderr)
        sys.exit(2)
    subs = [{"site_url": explicit_base, "site_name": explicit_base,
             "account_info": {"access_token": token}, "explicit": True}]
else:
    d = json.load(open(os.environ["HUB_FILE"]))
    subs = [a for a in d["accounts"]["accounts"] if a.get("site_type") == "sub2api"]
def exp(t):
    try:
        p = t.split(".")[1]; p += "=" * (-len(p) % 4)
        return json.loads(base64.urlsafe_b64decode(p)).get("exp")
    except Exception:
        return None
now = time.time()
# JWT 自称未过期的先试，但**不排除**其余的：exp 是声明，站点认不认是另一回事
subs.sort(key=lambda a: -( (exp(a.get("account_info", {}).get("access_token") or "") or 0) > now ))
tried = []
for a in subs:
    base = (a.get("site_url") or "").rstrip("/")
    tok = (a.get("account_info") or {}).get("access_token") or ""
    if not base or not tok:
        continue
    try:
        public = urllib.request.Request(base + "/api/v1/settings/public",
              headers={"Accept": "application/json", "User-Agent": "Go-http-client/2.0"})
        with urllib.request.urlopen(public, timeout=15) as r:
            settings = json.loads(r.read())
        setting_data = settings.get("data") if isinstance(settings, dict) else None
        if not isinstance(setting_data, dict) or not (
                setting_data.get("site_name") or "turnstile_enabled" in setting_data):
            tried.append(f"{base}: not-sub2api")
            continue
        explicit = bool(a.get("explicit"))
        if setting_data.get("turnstile_enabled") is True and not explicit:
            tried.append(f"{base}: turnstile-enabled")
            continue

        req = urllib.request.Request(base + "/api/v1/auth/me",
              headers={"Authorization": "Bearer " + tok, "Accept": "application/json",
                       "User-Agent": "Go-http-client/2.0"})
        with urllib.request.urlopen(req, timeout=15) as r:
            profile = json.loads(r.read())
        profile_data = profile.get("data") if isinstance(profile, dict) else None
        if not isinstance(profile_data, dict) or not profile_data.get("id"):
            tried.append(f"{base}: auth-not-profile")
            continue
        open(os.environ["TOKEN_FILE"], "w").write(tok)
        print(json.dumps({"base": base, "name": a.get("site_name") or base,
                          "user_id": str(profile_data.get("id")),
                          "tried": len(tried), "explicit": explicit}))
        sys.exit(0)
    except urllib.error.HTTPError as e:
        tried.append(f"{base}: HTTP {e.code}")
    except (json.JSONDecodeError, UnicodeDecodeError):
        tried.append(f"{base}: non-json")
    except Exception as e:
        tried.append(f"{base}: {type(e).__name__}")
print("导出里没有同时满足 Sub2API 指纹、无验证盾且 access_token 有效的站点：", file=sys.stderr)
for t in tried[:30]:
    print("   " + t, file=sys.stderr)
sys.exit(2)
PY
) || { echo "❌ 选站失败 —— 这不是被测代码的问题，是导出里没有可自动采集的 Sub2API 站点"; exit 2; }

BASE=$(python3 -c "import json,sys;print(json.loads(sys.argv[1])['base'])" "$PICK")
SITE=$(python3 -c "import json,sys;print(json.loads(sys.argv[1])['name'])" "$PICK")
UP_UID=$(python3 -c "import json,sys;print(json.loads(sys.argv[1])['user_id'])" "$PICK")
SKIP=$(python3 -c "import json,sys;print(json.loads(sys.argv[1])['tried'])" "$PICK")
EXPLICIT=$(python3 -c "import json,sys;print(json.loads(sys.argv[1])['explicit'])" "$PICK")
if [ "$EXPLICIT" = "True" ]; then
  echo "   ✅ 显式授权 ${SITE} <${BASE}>（已验证指纹与账号；仅验收，不改变导入规则）"
else
  echo "   ✅ 选中 ${SITE} <${BASE}>（前面 ${SKIP} 个候选未通过，跳过）"
fi

echo "── 2/6 起 PG 与 sla-core ──"
docker run -d --name "$CT" -e POSTGRES_PASSWORD=x -e POSTGRES_DB=sla \
  -p "${PORT}:5432" postgres:16 >/dev/null
for _ in $(seq 1 60); do
  docker exec "$CT" pg_isready -h 127.0.0.1 -U postgres -d sla >/dev/null 2>&1 && break
  sleep 1
done
docker exec "$CT" pg_isready -h 127.0.0.1 -U postgres -d sla >/dev/null 2>&1 || {
  echo "❌ PG 60s 内没起来"; docker logs "$CT" 2>&1 | tail -20 | sed 's/^/   /'; exit 1; }
sleep 2
go build -o bin/sla-core ./cmd/sla-core
DATABASE_URL="$DSN" ADMIN_TOKEN="$TOKEN" ./bin/sla-core -addr ":${APIPORT}" >"$LOG" 2>&1 &
CORE_PID=$!
for _ in $(seq 1 30); do
  curl -sf "http://127.0.0.1:${APIPORT}/healthz" >/dev/null 2>&1 && break
  sleep 1
done
curl -sf "http://127.0.0.1:${APIPORT}/healthz" >/dev/null 2>&1 || {
  echo "❌ sla-core 30s 内没就绪"; tail -20 "$LOG" | sed 's/^/   /'; exit 1; }
echo "   ✅ 就绪（PG :${PORT} / core :${APIPORT}）"

A="http://127.0.0.1:${APIPORT}/admin"
auth=(-H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json")
code() { : >"$RESP_JSON"; curl -s -o "$RESP_JSON" -w "%{http_code}" "$@"; }
fail() { echo "❌ $1"; echo "   响应: $(cat "$RESP_JSON")"; exit 1; }
psql_() { docker exec "$CT" psql -U postgres -d sla -tAc "$1"; }

echo "── 3/6 建渠道（auto_detect）并断言归族 ──"
C=$(code "${auth[@]}" -X POST "$A/channels" \
     -d "{\"name\":\"AC38-sub2api-${SITE}\",\"base_url\":\"${BASE}\",\"auto_detect\":true}")
[ "$C" = "201" ] || fail "建渠道应 201，得 $C"
CHID=$(python3 -c "import json,sys;print(json.load(open(sys.argv[1]))['id'])" "$RESP_JSON")
FAM=$(psql_ "SELECT site_family FROM channels WHERE id=$CHID")
[ "$FAM" = "sub2api" ] || fail "探测归族为 '$FAM'，应为 sub2api（AC-28 同口径）"
echo "   ✅ 渠道 #${CHID} 归族 sub2api"

# 从上游取回那把真 Key 登记进来。**不是造数据**：值来自 /api/v1/keys 的真实响应。
# 不这么做的话库里没有 upstream_keys 行，AC-38 的 quota_synced_at 那半就无从断言
# （不变式 N-1：本系统运行期不建 Key，Key 由人工登记）。
echo "── 4/6 建账号、登记凭证 + 那把真 Key（quota_synced_at 要有行才能前进）──"
python3 - "$BASE" "$TOKEN_FILE" "$KEY_JSON" <<'PY'
import json,sys,urllib.request
token=open(sys.argv[2]).read()
req=urllib.request.Request(sys.argv[1].rstrip("/")+"/api/v1/keys",
    headers={"Authorization":"Bearer "+token,"Accept":"application/json",
             "User-Agent":"Go-http-client/2.0"})
d=json.loads(urllib.request.urlopen(req,timeout=25).read())
items=(d.get("data") or {}).get("items") or []
k=items[0] if items else {}
json.dump({"secret":k.get("key",""),"ref":str(k.get("id",""))},
          open(sys.argv[3],"w"))
PY
SECRET=$(python3 -c "import json,sys;print(json.load(open(sys.argv[1]))['secret'])" "$KEY_JSON")
[ -n "$SECRET" ] || fail "上游 /api/v1/keys 没有可登记的 Key —— AC-38 的 quota 那半跑不了"
python3 -c "
import json,sys;json.dump({'channel_id':int(sys.argv[1]),'external_user_id':sys.argv[2]},
open(sys.argv[3],'w'))" "$CHID" "$UP_UID" "$ACC_JSON"
C=$(code "${auth[@]}" -X POST "$A/accounts" -d @"$ACC_JSON")
[ "$C" = "201" ] || fail "建账号应 201，得 $C"
ACCID=$(python3 -c "import json,sys;print(json.load(open(sys.argv[1]))['id'])" "$RESP_JSON")
python3 -c "
import json,sys
k=json.load(open(sys.argv[2]))
# 首轮同步前 channel_groups 为空；同步会先写分组，再把 Key 关联到该分组。
json.dump({'account_id':int(sys.argv[1]),'access_token':open(sys.argv[3]).read()},
          open(sys.argv[4],'w'))" "$ACCID" "$KEY_JSON" "$TOKEN_FILE" "$CRED_JSON"
C=$(code "${auth[@]}" -X POST "$A/collector/credentials" -d @"$CRED_JSON")
[ "$C" = "200" ] || fail "登记凭证应 200，得 $C"
python3 -c "
import json,sys
k=json.load(open(sys.argv[2]))
json.dump({'account_id':int(sys.argv[1]),'secret':k['secret'],'external_ref':k['ref']},
          open(sys.argv[3],'w'))" "$ACCID" "$KEY_JSON" "$KEYREQ_JSON"
C=$(code "${auth[@]}" -X POST "$A/keys" -d @"$KEYREQ_JSON")
[ "$C" = "201" ] || fail "登记 Key 应 201，得 $C"
echo "   ✅ 凭证 + 1 把真 Key 已登记"

echo "── 5/6 触发一次手动刷新（AC-38 本体）──"
C=$(code "${auth[@]}" -X POST "$A/channels/${CHID}/sync")
[ "$C" = "200" ] || fail "sync 应 200，得 $C"
cp "$RESP_JSON" "$SYNC_JSON"
python3 - "$SYNC_JSON" <<'PY' || exit 1
import json,sys
res=json.load(open(sys.argv[1]))
bad=[]
if not res.get("items"): bad.append("响应没有 items —— AC-38 要求逐项结果")
if not res.get("elapsed_ms"): bad.append("响应没有 elapsed_ms —— AC-38 要求带耗时")
for it in res.get("items",[]):
    for f in ("capability","support","status"):
        if not it.get(f): bad.append(f"某项缺 {f}：{it}")
    if "elapsed_ms" not in it: bad.append(f"项 {it.get('capability')} 缺 elapsed_ms")
print("   逐项：" + " ".join(
    f"{i.get('capability')}={i.get('support')}/{i.get('status')}/rows={i.get('rows',0)}"
    for i in res.get("items",[])))
for it in res.get("items",[]):
    cap, support, st = it.get("capability"), it.get("support"), it.get("status")
    rows, note = int(it.get("rows") or 0), it.get("note") or ""
    if support == "supported" and rows == 0:
        bad.append(f"{cap} supported 能力必须产生数据，实际 rows=0")
    if rows == 0:
        if support not in ("degraded", "unsupported"):
            bad.append(f"{cap} rows=0 时 support={support!r}，零行只允许 degraded/unsupported 且必须有说明")
        if not note:
            bad.append(f"{cap} rows=0 但缺 note，零行只允许 degraded/unsupported 且必须有说明")
    if st=="failed":
        bad.append(f"{cap} 失败：{next((i.get('error') for i in res['items'] if i['capability']==cap),'')}")
if bad:
    print("❌ AC-38 响应断言未过："); [print("   - "+b) for b in bad]; sys.exit(1)
print("   ✅ 逐项结果 + support + 耗时齐全；零行均有声明与说明，无 failed")
PY

echo "── 6/6 四类数据落库 + 限流 ──"
G=$(psql_ "SELECT count(*) FROM channel_groups WHERE channel_id=$CHID")
GM=$(psql_ "SELECT count(*) FROM group_models gm JOIN channel_groups g ON g.id=gm.channel_group_id WHERE g.channel_id=$CHID")
CAT=$(psql_ "SELECT count(*) FROM channel_model_catalog WHERE channel_id=$CHID")
QS=$(psql_ "SELECT count(*) FROM upstream_keys k JOIN upstream_accounts a ON a.id=k.account_id WHERE a.channel_id=$CHID AND k.quota_synced_at IS NOT NULL")
echo "   channel_groups=$G group_models=$GM channel_model_catalog=$CAT quota_synced_at 非空=$QS"
[ "$G" -gt 0 ]  || fail "channel_groups 无行"
[ "$QS" -gt 0 ] || fail "upstream_keys.quota_synced_at 未前进"
# ⚠️ 目录与分组模型**允许为 0 行，但不允许静默为 0**。
#
#    AC-38 的两条要求在真站点上会打架：「四类数据均更新」预设上游提供这四类，
#    而实测 Sub2API 站（0.1.183）的 /api/v1/groups/available 不含
#    available_models / models / supported_models 任一字段 —— 目录派生自它，
#    于是恒 0 行。内网真库佐证：13 个 sub2api 渠道合计 0 行，同库 newapi 是 2905。
#    **没有任何实现能在"上游不给"时把这两类填出来。**
#
#    故本条按 AC-38 的控制性条款判：**不得静默留空**。0 行必须带说明，
#    人才能区分"上游没给"与"我们写失败了"。判据落在 sync 响应的 note 上 ——
#    行数只在有数据时才是有效断言，拿它当唯一判据会把上游的缺失记成我们的失败。
python3 - "$GM" "$CAT" "$SYNC_JSON" <<'PYEOF' || exit 1
import json,sys
gm,cat=int(sys.argv[1]),int(sys.argv[2])
items={i["capability"]:i for i in json.load(open(sys.argv[3]))["items"]}
# ⚠️ 只查 model_catalog 那一项。groups 项的 rows 是**分组数**（本轮 8），
#    不是分组模型数 —— 拿 group_models=0 去要求 groups 项带 note，是把两个
#    不同的计数当成同一个（第一版就是这么写的，红在一条好路径上）。
#    分组模型与目录**同源**：都来自 available_models，故由目录那项统一说明。
it=items.get("model_catalog") or {}
if cat==0 and not it.get("note"):
    print("❌ model_catalog 落 0 行却没有任何说明 —— AC-38：不得静默留空")
    sys.exit(1)
if cat==0:
    print(f"   ⚠️ 模型目录 0 行，但已显式说明：{it['note'][:60]}…")
if gm==0:
    print("   ⚠️ 分组可用模型 0 行 —— 同源（available_models），见上一条")
PYEOF
C=$(code "${auth[@]}" -X POST "$A/channels/${CHID}/sync")
[ "$C" = "429" ] || fail "60s 内第二次 sync 应 429（09 §5.0bis），得 $C"
echo "   ✅ 有数据的三类均落库；0 行的两类均显式说明；第二次 sync 返回 429"

echo
echo "=========================================================="
echo "✅ AC-38 sub2api 一族通过：${SITE} <${BASE}>"
echo "   渠道 #${CHID} / 分组 ${G} / 分组模型 ${GM} / 目录 ${CAT} / 已同步 Key ${QS}"
