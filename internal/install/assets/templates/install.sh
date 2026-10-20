#!/bin/sh
# pkgdepot 目标机安装脚本。
#
# 目标机上只假设三样东西：sh、docker（含 compose v2 插件，缺则回退独立
# docker-compose）、tar（ADR-4）。脚本本身由 apply 通过 SSH 执行，参数全部来自
# 同目录的 apply.env；单独手工执行也可以，见文件末尾。
#
# 设计取舍：
#   - 幂等。重复 apply 是正常操作（增量补包），脚本必须能在已有部署上叠加：
#     npm/storage 与 python/packages 用 cp -a 合并而不是替换，容器用 up -d 收敛。
#   - 先校验再动手。镜像 tar 的 sha256 在 docker load 之前核对，避免把半个镜像
#     载进 daemon 后再在 compose up 阶段报一堆无关错误。
#   - 失败要能定位。每一步都打印编号与对象（哪个 tar、哪个文件），因为内网
#     排障时操作员往往只有一次远程会话。

set -eu

# 参数来自同目录的 apply.env（由 pkgdepot apply 生成）。显式传入的环境变量优先，
# 便于手工调试单步。compose 的 project 名由 baseDir 派生，使同一台机器上不同
# baseDir 的部署互不干扰（端口/容器名都不会撞）。
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
ENV_FILE="$SCRIPT_DIR/apply.env"
if [ -f "$ENV_FILE" ]; then
    # shellcheck disable=SC1090
    . "$ENV_FILE"
fi

BASE_DIR="${PKGDEPOT_BASE_DIR:-$SCRIPT_DIR}"
NPM_PORT="${PKGDEPOT_NPM_PORT:-4873}"
PYTHON_PORT="${PKGDEPOT_PYTHON_PORT:-8080}"
PROJECT="${PKGDEPOT_PROJECT:-pkgdepot}"
SKIP_WAIT="${PKGDEPOT_SKIP_WAIT:-0}"
BUNDLE_FILE="bundle.tar.gz"

log() { printf '[install] %s\n' "$*" >&2; }
die() { printf '[install] 失败: %s\n' "$*" >&2; exit 1; }

cd "$BASE_DIR" || die "无法进入 baseDir $BASE_DIR"

# --- 0. 前置检查 ---------------------------------------------------------
command -v docker >/dev/null 2>&1 || die "目标机没有 docker，pkgdepot 需要 docker 才能运行 registry"
docker info >/dev/null 2>&1 || die "docker daemon 不可用（docker info 失败）"

COMPOSE=""
if docker compose version >/dev/null 2>&1; then
    COMPOSE="docker compose"
elif command -v docker-compose >/dev/null 2>&1; then
    COMPOSE="docker-compose"
else
    die "没有 docker compose 插件，也没有 docker-compose 命令"
fi

[ -f compose.yaml ] || die "缺少 $BASE_DIR/compose.yaml"
[ -f "$BUNDLE_FILE" ] || die "缺少 $BASE_DIR/$BUNDLE_FILE（apply 应已上传）"

# --- 1. 解包 bundle -------------------------------------------------------
# 先解到工作目录，再把内容合并进 baseDir 的固定布局。直接解到 baseDir 会让
# bundle 的目录结构与 assets 写入的目录互相覆盖，顺序依赖变得不可推理。
WORK="$BASE_DIR/.unpack"
rm -rf "$WORK"
mkdir -p "$WORK"
log "1/6 解包 $BUNDLE_FILE"
tar -xzf "$BUNDLE_FILE" -C "$WORK" || die "解包 $BUNDLE_FILE 失败"

# npm 存储与 python 包目录是"增量叠加"的对象：保留目标机已有文件，只覆盖同名。
# 这样重复 apply 一个更小的 bundle 不会删掉上一次已经验证可用的包。
mkdir -p "$BASE_DIR/npm/storage" "$BASE_DIR/python/packages"
if [ -d "$WORK/npm/storage" ]; then
    log "2/6 合并 npm 存储（保留已有包，覆盖同名）"
    cp -a "$WORK/npm/storage/." "$BASE_DIR/npm/storage/"
fi
if [ -d "$WORK/python/packages" ]; then
    log "2/6 合并 python 包目录"
    cp -a "$WORK/python/packages/." "$BASE_DIR/python/packages/"
fi

# 镜像 tar 也合并进 baseDir/images：apply 侧已按 manifest 写好对应的
# <file>.sha256，重复执行 install.sh 时仍可自校验。
mkdir -p "$BASE_DIR/images"
if [ -d "$WORK/images" ]; then
    cp -a "$WORK/images/." "$BASE_DIR/images/"
fi

# 权威清单：后续 verify/status 都读它。
if [ -f "$WORK/manifest.json" ]; then
    cp -f "$WORK/manifest.json" "$BASE_DIR/manifest.json"
fi

# verdaccio 6 要求 storage 目录可写（以 root 运行由 compose 保证，这里只补权限位）。
chmod -R u+rwX "$BASE_DIR/npm/storage" 2>/dev/null || true
# pypiserver 以非 root 用户运行，packages 目录必须对所有用户可读可进入，
# 否则 /simple/<name>/ 会因为无法计算哈希而 500（已实测）。
chmod -R a+rX "$BASE_DIR/python/packages" 2>/dev/null || true
# htpasswd 文件必须存在，否则 verdaccio 在 auth 初始化时报错。内容为空即可：
# 内网不启用密码登录，auth 段只是为了满足配置结构。
mkdir -p "$BASE_DIR/npm/conf"
[ -f "$BASE_DIR/npm/conf/htpasswd" ] || : > "$BASE_DIR/npm/conf/htpasswd"

# --- 2. 加载镜像 ----------------------------------------------------------
# 离线机上 docker load 是唯一来源。逐个文件校验 sha256 后加载，失败的 tar
# 直接点名，避免 compose up 时报 "No such image" 却不知道是哪个包坏了。
log "3/6 加载镜像"
[ -d "$BASE_DIR/images" ] || die "缺少 $BASE_DIR/images（bundle 应包含镜像 tar）"
found_image=0
for tar_file in "$BASE_DIR"/images/*.tar; do
    [ -e "$tar_file" ] || continue
    found_image=1
    base="$(basename "$tar_file")"
    sum_file="$BASE_DIR/images/$base.sha256"
    if [ -f "$sum_file" ]; then
        want="$(cut -d' ' -f1 < "$sum_file")"
        got="$(sha256sum "$tar_file" | cut -d' ' -f1)"
        [ "$want" = "$got" ] || die "镜像 $base 校验和不匹配（期望 $want，实际 $got），bundle 可能损坏"
    fi
    log "      docker load -i images/$base"
    docker load -i "$tar_file" >/dev/null || die "docker load $base 失败"
done
[ "$found_image" -eq 1 ] || die "$BASE_DIR/images 下没有 .tar 镜像文件"

# --- 3. 启动容器 ----------------------------------------------------------
log "4/6 docker compose up -d（npm:$NPM_PORT python:$PYTHON_PORT）"
# --remove-orphans：上一次 apply 留下的旧服务不应继续占端口。
# --force-recreate 不是保守起见，而是必须：bind mount 在容器创建时就被解析成
# 具体 inode，如果 baseDir 被删掉重建（清空重来、换盘、恢复快照），旧容器仍然
# 挂在已经不存在的目录上——服务看起来 running，registry 里却一个包都没有。
# compose 只按配置哈希判断"无需重建"，发现不了这种悬空挂载（已实测）。每次
# apply 重建容器的代价是几秒停机，数据都在 bind mount 里不会丢。
$COMPOSE -f compose.yaml -p "$PROJECT" up -d --remove-orphans --force-recreate >/dev/null || die "docker compose up 失败"

# --- 4. 等待健康 ----------------------------------------------------------
# 以容器 healthcheck 为主要信号，端口探测作补充：healthcheck 能区分
# "进程在但服务没起来"（pypiserver 的 /simple/ 500 就是这种）。
wait_health() {
    service="$1"; port="$2"; path="$3"; deadline="$4"
    elapsed=0
    while [ "$elapsed" -lt "$deadline" ]; do
        cid="$($COMPOSE -f compose.yaml -p "$PROJECT" ps -q "$service" 2>/dev/null || true)"
        if [ -n "$cid" ]; then
            status="$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "$cid" 2>/dev/null || echo unknown)"
            case "$status" in
                healthy) log "      $service 已健康"; return 0 ;;
                unhealthy) log "      $service 报告 unhealthy，继续等待（可能是初次启动慢）" ;;
            esac
        fi
        # 端口探测走 127.0.0.1：目标机上 docker 已发布端口即可，不依赖外部网络。
        if command -v wget >/dev/null 2>&1 && wget -q -O /dev/null "http://127.0.0.1:$port$path" 2>/dev/null; then
            log "      $service 端口 $port 已响应"; return 0
        fi
        sleep 2
        elapsed=$((elapsed + 2))
    done
    return 1
}

if [ "$SKIP_WAIT" != "1" ]; then
    log "5/6 等待 registry 就绪"
    wait_health verdaccio "$NPM_PORT" "/-/ping" 180 || die "verdaccio 在 180s 内没有就绪（检查 docker compose -p pkgdepot logs verdaccio）"
    wait_health pypiserver "$PYTHON_PORT" "/simple/" 180 || die "pypiserver 在 180s 内没有就绪（检查 docker compose -p pkgdepot logs pypiserver）"
else
    log "5/6 跳过就绪等待（PKGDEPOT_SKIP_WAIT=1）"
fi

# 把容器状态落盘，供 status 子命令在离线时也能读到最近一次 apply 的结果。
$COMPOSE -f compose.yaml -p "$PROJECT" ps > "$BASE_DIR/containers.txt" 2>/dev/null || true

log "6/6 完成：npm http://127.0.0.1:$NPM_PORT/  python http://127.0.0.1:$PYTHON_PORT/simple/"
exit 0
