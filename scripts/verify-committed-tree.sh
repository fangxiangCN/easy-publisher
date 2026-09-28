#!/usr/bin/env bash
#
# 验证「已提交的树」能构建，而不是工作树。
#
# 为什么需要这个脚本：.gitignore 里的 *credential* 模式静默排除了
# core/config/CredentialStore.kt。本地 ./gradlew build 一直是绿的（文件在磁盘上），
# 而远端仓库从初始提交起就编译不过，持续了 7 次推送才被用户发现（cf52205）。
#
# 单元测试防不住这类问题 —— 它们跑的也是工作树。唯一可靠的办法是克隆一份
# 已提交的内容再构建。每个阶段结束前跑一次。
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

echo "==> [1/3] 检查是否有源码文件被 .gitignore 误伤"
# 只看代码文件：这类文件被忽略必然是配置错误。
# 运行时产物（build/、.gradle/、apps/*.json 等）不在检查范围内。
ignored_code="$(git -C "$REPO" status --ignored --short \
  | grep '^!!' | awk '{print $2}' \
  | grep -E '\.(kt|kts|go|java|mod|sum)$' || true)"
if [ -n "$ignored_code" ]; then
  echo "!! 以下源码文件被 .gitignore 排除，不会进入仓库："
  echo "$ignored_code" | sed 's/^/     /'
  echo ""
  echo "   用 git check-ignore -v <文件> 查是哪条规则命中的。"
  exit 1
fi
echo "    ok"

echo "==> [2/3] 克隆已提交的树"
git -C "$REPO" clone --quiet --no-hardlinks "$REPO" "$WORK/tree"
cd "$WORK/tree"
echo "    HEAD: $(git rev-parse --short HEAD)"

echo "==> [3/3] 构建"
./gradlew build --no-daemon --quiet
echo "    Kotlin: ok"

if [ -d go ] && [ -n "$(find go -name '*.go' -print -quit 2>/dev/null)" ]; then
  if command -v go >/dev/null 2>&1; then
    (cd go && go build ./... && go vet ./... && go test ./...)
    echo "    Go: ok"
  else
    # 环境问题而非仓库问题，所以只警告不失败。
    # 但要说清楚：Go 代码存在却没被验证过，「已提交的树可构建」这句话此时只覆盖 Kotlin。
    echo "    Go: 跳过 —— 未安装 go 工具链，go/ 下的代码未经构建验证" >&2
    GO_VERIFIED=0
  fi
fi

echo ""
if [ "${GO_VERIFIED:-1}" = "1" ]; then
  echo "==> 已提交的树可构建"
else
  echo "==> 已提交的树可构建（仅 Kotlin；Go 部分未验证）"
fi
