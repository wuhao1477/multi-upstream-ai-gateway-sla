#!/usr/bin/env bash
# PM 文档交付门禁：文档一致性 + 覆盖率报告分类自测 + DDL 真跑。CI 与本地都跑它。
set -euo pipefail
cd "$(dirname "$0")/.."
echo "── 1/3 文档一致性 ──"
python3 verify/check_docs.py
echo
# 这一步能进 CI 是因为它**不需要任何真依赖**：断言串取自已归档的覆盖率报告
# （docs/acceptance/coverage/*.json，随仓库走），不打上游也不连库。
# 值得进门禁的理由见 P1-evidence §5.16：分类顺序错了不会抛异常、不会让报告跑不出来，
# 它只是安静地把 TLS 超时记成"凭证失效"，再把那个站列进人工清单。
echo "── 2/3 覆盖率报告的失败分类（判定顺序）──"
python3 verify/test_coverage_classify.py
echo
echo "── 3/3 DDL 可执行性（postgres:16）──"
./verify/ddl-check.sh
