# tvbox-manager — TVBox 源维护系统

自托管的 TVBox 类仓库源管理系统：录入源地址、实时预览、定时探活、启停管理，
对外只暴露**一个订阅地址**，三方客户端（TVBox / OK影视 / 影视仓 / 猫影视等）填入即可获得当前启用的多仓源。

解决三个痛点：

1. 源地址散落各处，靠记事本/收藏夹维护，容易乱、容易忘
2. 不知道哪个源还活着，失效源填进客户端表现为「一直转圈」，排查成本高
3. 客户端只能填一个地址，换源要手动改

技术形态：**Go 单二进制**（纯标准库 + `golang.org/x/net` IDN），数据为 **JSON 文件**（原子写入，可文本编辑器直接查看/备份），
前端 **Vue3 内嵌**（go:embed，完全离线可用，不依赖任何 CDN）。

## 快速开始

### Docker（推荐）

```bash
docker run -d --name tvbox-manager \
  -p 8080:8080 \
  -e TOKEN=你的令牌 \
  -e TZ=Asia/Shanghai \
  -v /opt/tvbox/data:/data \
  --restart unless-stopped \
  tvbox-manager:latest
```

或用 compose：

```bash
docker compose up -d
```

启动后：

- 管理后台：<http://服务器IP:8080/>（首次启动若无 TOKEN 环境变量，会随机生成并打印在容器日志中）
- 订阅地址：<http://服务器IP:8080/sub> ← **这个填进 TVBox 客户端**

### 本机直接运行

```bash
go build -trimpath -ldflags="-s -w" -o tvbox-manager .
DATA_DIR=./data TOKEN=你的令牌 ./tvbox-manager
```

Windows 下用 `tvbox-manager.exe`，环境变量写法 `set DATA_DIR=...` 或 PowerShell `$env:DATA_DIR=...`。

## 功能

| 功能 | 说明 |
|---|---|
| 源管理 | 增删改查、分组、排序、备注、启用/禁用（仅启用的进入订阅） |
| 批量录入 | 粘贴多行（`名称,URL` / `名称\|URL` / 纯 URL 混排）、.json / .txt 文件导入（前端预览勾选） |
| 源类型识别 | 单仓（sites/lives/parses）/ 多仓（urls），多仓优先；结构摘要（站点数、直播数、解析数、spider、壁纸、站点名清单、子仓列表） |
| 实时预览 | 点一下就知道这个地址**现在**能不能用、里面有什么；原始 JSON 可复制/展开 |
| 探活 | 默认每 6 小时全量（可配），手动触发（运行中再触发返回 409），进度实时上报；失败原因分类：超时 / 域名无法解析 / 连接被拒绝 / 证书校验失败 / HTTP 状态码 / 响应非 JSON / 仓库结构不完整 |
| 自动禁用 | 连续失败 N 次自动禁用（默认关，0=不自动） |
| 子仓递归探活 | 多仓的 urls[] 逐个探活（可选，默认关） |
| 订阅输出 | `/sub`（多仓 JSON）、`?format=fan`（饭太硬风格）、`/sub/txt`（纯文本）、`?name=前缀` |
| 备份 | 导出/导入（merge 合并 / replace 替换），Token 永不导出 |
| 安全 | 后台 Token 鉴权（X-Token / Bearer / ?token=，常数时间比较）、`/api/preview?url=` 内网地址黑名单（SSRF 防护）、请求体 10MB / 上传 2MB 限制 |

### 真实源兼容性（实测社区源得出的数据格式经验）

- **中文国际域名**：`肥猫.live` 自动 IDN→Punycode；无协议前缀自动补 `http://`
- **宽松 JSON**：`//` 注释、`/* */` 块注释、尾逗号、单引号字符串、BOM、正文前的公告杂文字——词法安全剥离后仍可解析（字符串字面量里的 `//` 不会被误剥）
- **多仓 urls 兼容**：对象 `{url,name}` 与纯字符串混排；`csp_XPathhttp://...` 类格式前缀自动剥离；空地址跳过
- **加密源**：`$#` 混淆壳等加密内容明确报「响应非 JSON 格式」，不会误判为可用

## 探活准确率的两个关键默认值

1. **UA 默认 `okhttp/4.9.1`**：大量 TVBox 源站对浏览器 UA 返回 403，模拟客户端 UA 才能拿到真实结果。可在设置页修改。
2. **默认跳过 TLS 证书校验**：大量三方源用自签/过期证书的 HTTPS，关闭校验才能判断「源本身是否可用」而不是被证书问题误判。若部署在受控内网可在设置页关闭（请知悉风险）。

## API 摘要

统一信封 `{"code":0,"msg":"ok","data":...}`；除 `/healthz` 与订阅输出外，`/api/*` 均需 Token。

| 方法 | 路径 | 说明 |
|---|---|---|
| GET/POST | `/api/sources` | 列表（keyword/type/status/group/enabled/page/sort 筛选）/ 新增（autoCheck 默认异步探活） |
| POST | `/api/sources/batch` | 批量新增，`onDuplicate: skip(默认)/update` |
| POST | `/api/sources/import` | 文件导入（multipart 字段 file，.json/.txt ≤2MB） |
| GET/PUT/DELETE | `/api/sources/{id}` | 详情 / 修改 / 删除 |
| POST | `/api/sources/{id}/toggle` · `/{id}/check` | 切换启用 / 单源探活 |
| POST | `/api/sources/batch-action` | 批量 enable/disable/delete/group |
| GET | `/api/sources/{id}/preview` · `/api/preview?url=` | 实时预览（后者有 SSRF 内网黑名单） |
| POST/GET | `/api/check/run` · `/api/check/status` | 全量探活（运行中 409）· 进度 |
| GET | `/api/stats` · `/api/groups` | 概览统计 · 分组列表 |
| GET/PUT | `/api/config` · `POST /api/config/token` | 配置（Token 脱敏 `**`）/ 修改令牌 |
| GET/POST | `/api/export` · `/api/import/config` | 备份导出 / 导入 |
| GET | `/healthz` | 存活探针 |
| GET | `/sub` | 订阅（免鉴权）：`?name=前缀`、`?format=fan`、`/sub/txt`；`/sub/all` 全量需 Token |

## 环境变量（> config.json > 默认值）

| 变量 | 默认 | 说明 |
|---|---|---|
| `PORT` | 8080 | 监听端口 |
| `TOKEN` | 随机生成 | 后台访问令牌（首启打印在日志） |
| `DATA_DIR` | /data（Docker）/ ./data | 数据目录（data.json + config.json） |
| `SUB_PATH` | /sub | 订阅路径（改后重启生效） |
| `CHECK_INTERVAL` | 360 | 探活间隔（分钟） |
| `CHECK_TIMEOUT` | 15 | 单请求超时（秒） |
| `CHECK_CONCURRENCY` | 10 | 探活并发 |
| `STARTUP_CHECK` | false | 启动即全量探活 |
| `AUTO_DISABLE_FAILS` | 0 | 连续失败自动禁用阈值 |
| `INSECURE_TLS` | true | 跳过证书校验 |
| `USER_AGENT` | okhttp/4.9.1 | 探活 UA |
| `CHECK_SUB_URLS` | false | 递归探活多仓子仓 |
| `TZ` | Asia/Shanghai | 时区 |

## 安全须知

- **订阅接口无鉴权是设计取舍**（客户端填不了 Token）。任何知道地址的人都能拉到订阅列表。
  公网部署请用反向代理加 IP 白名单 / Basic Auth，或仅内网暴露端口。
- `/api/preview?url=` 已内置 SSRF 防护：拒绝环回/内网/链路本地地址（127/8、10/8、172.16/12、192.168/16、169.254/16、::1、fc00::/7 等）。
  已入库源的探活/预览不做内网拦截——NAS 内网自建源属于正常使用场景。
- 日志不打印 Token；`GET /api/config` 返回脱敏值。
- data.json 损坏时自动改名备份为 `data.json.corrupt.时间戳` 并以空数据启动，不覆盖不丢数据。

## 多架构构建（NAS 常为 arm64）

```bash
docker buildx build --platform linux/amd64,linux/arm64,linux/arm/v7 \
  -t yourname/tvbox-manager:latest --push .
```

本机没有 Docker 也能编译（宿主只需 Docker）：

```bash
docker run --rm -v "$PWD":/src -w /src -e CGO_ENABLED=0 \
  golang:1.23-alpine sh -c "go mod download && go build -ldflags='-s -w' -o bin/tvbox-manager ."
```

## 开发

```bash
go test ./...        # 4 个包全绿（parser / store / checker / api 端到端）
go build .
```

数据模型与模块划分见 `internal/`：`model`（结构体）/ `store`（JSON 持久化+原子写）/ `parser`（类型识别+归一化+宽松 JSON）/ `checker`（探活+错误分类+SSRF）/ `scheduler`（定时调度）/ `api`（路由+鉴权+订阅输出）。前端在 `web/`（原生 Vue3，无构建步骤，`go:embed` 内嵌）。

## 法律提示

本工具仅做**地址管理与可用性检测**，不存储、不转发、不解析任何视频内容。三方源的版权责任由源提供方承担。仅供个人学习与技术交流使用。
