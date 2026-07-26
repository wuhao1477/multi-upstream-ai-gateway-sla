#!/usr/bin/env bash
# PM 文档交付门禁：文档一致性 + DDL 真跑。CI 与本地都跑它。
set -euo pipefail
cd "$(dirname "$0")/.."
echo "── 1/2 文档一致性 ──"
python3 verify/check_docs.py
echo
echo "── 2/2 DDL 可执行性（postgres:16）──"
./verify/ddl-check.sh
