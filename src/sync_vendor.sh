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

# 以下文件是 addon 独有、手维护，sync 时绝不覆盖（保留本地版本）
# 用数组 + 逐个比对，避免把换行塞进 case 模式导致匹配失效（旧写法静默失效过）
PROTECT_FILES=(
  "internal/login_trae/addon_extras.go"
  "internal/svc/svc.go"
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
