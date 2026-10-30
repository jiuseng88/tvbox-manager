#!/usr/bin/env bash
# 构建 tvbox-manager 镜像（多架构）。
#
# 用法：
#   ./scripts/build-image.sh                 # 构建 amd64 + arm64 + armv7 并推送
#   ./scripts/build-image.sh --load          # 只构建当前平台并载入本地 docker（用于本机测试）
#   ./scripts/build-image.sh --no-push       # 构建但只留在 buildx 缓存里（不推送）
#   TAG=1.4.7 ./scripts/build-image.sh       # 指定 tag（默认同时打 latest 与版本号）
#   PLATFORMS=linux/amd64 ./scripts/build-image.sh
#
# 环境变量：
#   IMAGE       镜像仓库地址（默认阿里云 qdjzw/jiuseng）
#   TAG         版本 tag（默认取 main.go 里的 const version）
#   PLATFORMS   目标平台（默认 linux/amd64,linux/arm64,linux/arm/v7）
set -euo pipefail

cd "$(dirname "$0")/.."

IMAGE="${IMAGE:-registry.cn-hangzhou.aliyuncs.com/qdjzw/jiuseng}"
PLATFORMS="${PLATFORMS:-linux/amd64,linux/arm64,linux/arm/v7}"
VERSION="${TAG:-$(sed -n 's/^const version = "\(.*\)"$/\1/p' main.go)}"
PUSH=1
LOAD=0

for arg in "$@"; do
  case "$arg" in
    --push)    PUSH=1 ;;
    --no-push) PUSH=0 ;;
    --load)    LOAD=1; PUSH=0; PLATFORMS="linux/amd64" ;;
    -h|--help) sed -n '2,20p' "$0"; exit 0 ;;
    *) echo "未知参数: $arg（用 --help 查看用法）" >&2; exit 2 ;;
  esac
done

if ! command -v docker >/dev/null 2>&1; then
  echo "错误：找不到 docker 命令。镜像构建需要 Docker（Linux 服务器 / Docker Desktop / 或任意有 buildx 的机器）。" >&2
  exit 1
fi

if [ -z "$VERSION" ]; then
  echo "错误：无法从 main.go 读取版本号，请用 TAG=1.4.7 显式指定" >&2
  exit 1
fi

echo "镜像    : $IMAGE:$VERSION"
echo "平台    : $PLATFORMS"
echo "推送    : $([ "$PUSH" = 1 ] && echo 是 || echo 否)"
echo "版本    : $VERSION"
echo

# 多架构构建需要一个支持多平台的 builder，不存在就建一个
if [ "$LOAD" = 0 ]; then
  if ! docker buildx inspect tvbox-builder >/dev/null 2>&1; then
    echo ">> 创建 buildx builder: tvbox-builder"
    docker buildx create --name tvbox-builder --driver docker-container >/dev/null
  fi
  docker buildx use tvbox-builder >/dev/null
  docker buildx inspect --bootstrap >/dev/null 2>&1 || true
fi

ARGS=(
  build
  --platform "$PLATFORMS"
  -t "$IMAGE:$VERSION"
  -t "$IMAGE:latest"
  # 关掉 attestation：部分个人版镜像仓库不接受 manifest list 里的 provenance 层
  --provenance=false
  .
)

if [ "$LOAD" = 1 ]; then
  ARGS+=(--load)
elif [ "$PUSH" = 1 ]; then
  ARGS+=(--push)
fi

echo ">> docker buildx ${ARGS[*]}"
docker buildx "${ARGS[@]}"

echo
echo "完成：$IMAGE:$VERSION / $IMAGE:latest"
if [ "$PUSH" = 1 ]; then
  echo "拉取测试： docker pull $IMAGE:$VERSION"
  echo "校验架构： docker buildx imagetools inspect $IMAGE:$VERSION"
fi
