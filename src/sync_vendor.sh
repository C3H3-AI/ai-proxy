#!/usr/bin/env bash
# sync_vendor.sh - 从上游 wild-work 生成式同步 addon 的同源 internal 包。
#
# 用法:  cd D:/ai-hub/integrations/ha-ai-proxy/src && ./sync_vendor.sh
#
# 行为:
#   1. 读取上游克隆目录 (UPSTREAM_DIR) 的 HEAD commit
#   2. 复制同源包到 src/internal/ (带 GENERATED 头)
#   3. 跳过 addon 独有包: svc, trae_adaptor
#   4. 输出变更列表
#
# 注意: 本脚本只同步"纯上游副本"。任何 addon 特有逻辑必须放在 trae_adaptor/ 或 svc/，
#       不要手写改 generated 文件，否则下次 sync 会被覆盖。

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ADDON_INTERNAL="${SCRIPT_DIR}/internal"
UPSTREAM_DIR="${UPSTREAM_DIR:-D:/ai-hub/_upstream-wild-work}"

if [ ! -d "${UPSTREAM_DIR}/.git" ]; then
  echo "ERROR: upstream clone missing: ${UPSTREAM_DIR}" >&2
  echo "       git clone https://github.com/rockswang/wild-work ${UPSTREAM_DIR}" >&2
  exit 1
fi

COMMIT="$(cd "${UPSTREAM_DIR}" && git rev-parse --short HEAD)"
UP_INTERNAL="${UPSTREAM_DIR}/internal"
SYNC_PKGS="auth config login login_qoder login_trae pool provider qoder scheduler server traework upstream"

# 上次实际同步到的上游 commit。用于判断「上游自基线后是否改动过受保护文件」。
# 同步成功且人工确认无遗漏后，把这里更新为新的 COMMIT。
# 见 SYNC.md「对齐基线」。
BASELINE_COMMIT="c62d0bc"

# ── 受保护文件（sync 时不覆盖，保留 addon 版本）─────────────────────────────
#
# 分两类，原因不同：
#
# 【A 类】addon 独有文件 —— 上游根本没有这些文件，PROTECT 只是显式声明。
#
# 【B 类】addon 修改过的上游文件 —— 这些文件上游有，但 addon 做了**无法外移**的改动。
#         为何无法外移（已逐项验证）：
#           - pool.go : 改了 Pick()/pickExcluding() 的**内部实现**。Go 不支持方法覆写，
#                       同名函数放同包新文件会直接编译冲突；而所需状态
#                       (lowCredit/lowCredits) 是**私有字段**，外部包访问不了。
#           - config.go: 新增 LowCreditThreshold 字段，被 svc.go 引用。
#           - server/handler.go: TraeWork 预刷新窗口按平台区分（defaultRefreshSkew）。
#           - upstream/client.go: 流式请求改用无总时长上限的 client（StreamHTTP）。
#
# ⚠️ B 类的代价：这些文件**不再跟随上游自动更新**。上游若改动了它们，
#    本脚本会在结尾**显式报告**（不会静默跳过），届时需人工 merge。
PROTECT_FILES=(
  # A 类：addon 独有
  "internal/login_trae/addon_extras.go"
  "internal/svc/svc.go"
  # B 类：addon 修改过的上游文件
  "internal/pool/pool.go"
  "internal/config/config.go"
  "internal/server/handler.go"
  "internal/upstream/client.go"
  "internal/upstream/headers.go"
  "internal/traework/constants.go"
)

# B 类保护文件清单：用于结尾的上游差异检查。
# 只对这些文件检查「上游相对当前 addon 副本是否已变化」。
CHECK_UPSTREAM_FILES=(
  "internal/pool/pool.go"
  "internal/config/config.go"
  "internal/server/handler.go"
  "internal/upstream/client.go"
  "internal/upstream/headers.go"
  "internal/traework/constants.go"
)

echo "==> upstream HEAD: ${COMMIT}"
echo "==> syncing: ${SYNC_PKGS}"

CHANGED=0
for pkg in $SYNC_PKGS; do
  src_pkg="${UP_INTERNAL}/${pkg}"
  dst_pkg="${ADDON_INTERNAL}/${pkg}"
  [ -d "${src_pkg}" ] || { echo "    [skip] no upstream pkg ${pkg}"; continue; }
  mkdir -p "${dst_pkg}"
  for f in $(find "${src_pkg}" -name "*.go" -not -name "*_test.go"); do
    rel="${f#${src_pkg}/}"
    dst="${dst_pkg}/${rel}"
    # 跳过 addon 独有文件（手维护，不被 upstream 覆盖）
    protected=0
    for pf in "${PROTECT_FILES[@]}"; do
      case "${dst}" in *"${pf}") protected=1; break ;; esac
    done
    if [ "${protected}" -eq 1 ]; then echo "    [keep] ${dst#${ADDON_INTERNAL}/}"; continue; fi
    mkdir -p "$(dirname "${dst}")"
    { echo "// CODE GENERATED FROM wild-work@${COMMIT} -- DO NOT EDIT, run sync_vendor.sh"; echo; sed "s#wild-work/internal#github.com/rockswang/workbuddy-wild/internal#g" "${f}"; } > "${dst}.tmp"
    if [ -f "${dst}" ] && diff -q "${dst}" "${dst}.tmp" >/dev/null 2>&1; then
      rm -f "${dst}.tmp"
    else
      mv "${dst}.tmp" "${dst}"
      echo "    [update] internal/${pkg}/${rel}"
      CHANGED=$((CHANGED+1))
    fi
  done
done

echo "==> done: ${CHANGED} files changed (from wild-work@${COMMIT})"
if [ "${CHANGED}" -gt 0 ]; then echo "    run: go build ./... && go vet ./internal/..."; fi

# ── B 类保护文件的上游差异检查 ────────────────────────────────────────────
#
# 受保护文件不会被覆盖，但**必须让人知道上游是否动过它们** ——
# 否则 PROTECT 就成了黑洞：上游的修复/改动被永久静默忽略。
#
# 判断方式：把上游当前版本按 sync 的同样方式规范化（去 CRLF、改 module 前缀、
# 去掉 GENERATED 头），与 addon 本地版本比较。若**上游自身**相对上次同步基线
# 发生了变化，本地副本与上游的差异就会超出 addon 自有改动，此处给出提示。
#
# 注意：上游文件是 CRLF、addon 是 LF，比较前必须 tr -d '\r'，
# 否则全文件都会显示为「有差异」而失去意义（这个坑踩过）。
echo
echo "==> 受保护文件（B 类）上游差异检查"
NEED_MERGE=0
for pf in "${CHECK_UPSTREAM_FILES[@]}"; do
  up_file="${UP_INTERNAL}/${pf#internal/}"
  ad_file="${ADDON_INTERNAL}/${pf#internal/}"
  if [ ! -f "${up_file}" ]; then
    echo "    [n/a ] ${pf} — 上游已无此文件，请确认是否应移除"
    continue
  fi
  if [ ! -f "${ad_file}" ]; then
    echo "    [n/a ] ${pf} — addon 本地不存在"
    continue
  fi
  # 规范化上游版本：去 CRLF、改 module 前缀
  norm_up="$(mktemp)"
  sed "s#wild-work/internal#github.com/rockswang/workbuddy-wild/internal#g" "${up_file}" | tr -d '\r' > "${norm_up}"
  # addon 版本：去掉 GENERATED 头两行
  norm_ad="$(mktemp)"
  tail -n +3 "${ad_file}" > "${norm_ad}"

  diff_lines=$(diff "${norm_up}" "${norm_ad}" 2>/dev/null | grep -c '^[<>]' || true)
  rm -f "${norm_up}" "${norm_ad}"

  if [ "${diff_lines}" -eq 0 ]; then
    echo "    [ok  ] ${pf} — 与上游一致（addon 未改动或已同步）"
  else
    # 有差异是正常的（addon 自有改动）。真正要提示的是「上游在基线之后动过它」。
    if [ -n "${BASELINE_COMMIT:-}" ]; then
      up_changed=$(cd "${UPSTREAM_DIR}" && git log --oneline "${BASELINE_COMMIT}..${COMMIT}" -- "internal/${pf#internal/}" 2>/dev/null | wc -l || echo 0)
      if [ "${up_changed}" -gt 0 ]; then
        echo "    [WARN] ${pf} — 上游自基线 ${BASELINE_COMMIT} 起改动了 ${up_changed} 次，需人工 merge"
        NEED_MERGE=$((NEED_MERGE+1))
      else
        echo "    [keep] ${pf} — 差异 ${diff_lines} 行（addon 自有改动；上游未动）"
      fi
    else
      echo "    [keep] ${pf} — 差异 ${diff_lines} 行（addon 自有改动）"
    fi
  fi
done

if [ "${NEED_MERGE}" -gt 0 ]; then
  echo
  echo "⚠️  有 ${NEED_MERGE} 个受保护文件上游已改动，请人工 merge 后再提交。"
  echo "    （这些文件不会自动同步，是设计使然：见 PROTECT_FILES 注释）"
fi
