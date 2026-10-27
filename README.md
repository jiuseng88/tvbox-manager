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

镜像已发布到阿里云容器镜像服务，可直接拉取：

```bash
docker pull registry.cn-hangzhou.aliyuncs.com/qdjzw/jiuseng:latest
```

```bash
# 绑定挂载的数据目录要先建好并改属主：容器内以 app 用户（uid=100 gid=101）运行
mkdir -p /opt/tvbox/data && chown -R 100:101 /opt/tvbox/data

docker run -d --name tvbox-manager \
  -p 8080:8080 \
  -e TOKEN=你的令牌 \
  -e PORT=8080 \
  -e SUB_PATH=/sub \
  -e TZ=Asia/Shanghai \
  -v /opt/tvbox/data:/data \
  --restart unless-stopped \
  registry.cn-hangzhou.aliyuncs.com/qdjzw/jiuseng:latest
```

`TOKEN` / `PORT` / `SUB_PATH` 在**首次启动**时作为初始值写入 `data/config.json`；
之后在后台「设置」页改即可，配置文件优先于环境变量（详见下方「环境变量」一节）。
改 `PORT` 时记得同步改 `-p 宿主机端口:容器端口` 的容器端口。

或用 compose（默认从上面这个地址拉取镜像，不再本地构建；数据用命名卷 `tvbox-data`，
首次挂载自动继承容器内属主，无需 chown）：

```bash
docker compose up -d
```

数据在 `/var/lib/docker/volumes/<项目名>_tvbox-data/_data`，备份：
`docker run --rm -v tvbox-data:/data -v $PWD:/backup alpine tar czf /backup/data.tgz /data`。

> 提示：compose 里如果从命名卷切回 `./data:/data` 这类绑定挂载，先
> `mkdir -p ./data && chown -R 100:101 ./data`（CentOS 上 SELinux enforcing 时再加 `:z`）。

要从源码构建镜像，用仓库根目录的 `Dockerfile`（多阶段，需能拉 `golang:1.27-alpine`）：
`docker build -t tvbox-manager:latest .`，然后把 compose 里的 `image` 换成 `tvbox-manager:latest` 并启用 `build: .`。

启动后：

- 管理后台：<http://服务器IP:8080/>（未指定 `TOKEN` 时会随机生成，并打印在启动日志的「访问令牌」一行）
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
| 批量录入 | 粘贴多行（`名称,URL` / `名称\|URL` / 纯 URL 混排）、.json / .txt 文件导入（前端预览勾选）；**导入后默认后台批量探活**（并发受限、攒批落盘），可用 `autoCheck:false` 关掉 |
| 源类型识别 | 单仓（sites/lives/parses）/ 多仓（urls），多仓优先；结构摘要（站点数、直播数、解析数、spider、壁纸、站点名清单、子仓列表） |
| 实时预览 | 点一下就知道这个地址**现在**能不能用、里面有什么；原始 JSON 可复制/展开 |
| 探活 | 默认每 6 小时全量（可配），覆盖**父源 + 站点源**两轮（同地址只请求一次，直接复用结果），手动触发（运行中再触发返回 409），进度实时上报；失败原因分类：超时 / 域名无法解析 / 连接被拒绝 / 证书校验失败 / HTTP 状态码 / 响应非 JSON / 仓库结构不完整 |
| 自动禁用 | 连续失败 N 次自动禁用（默认关，0=不自动；**父源与站点源同口径**，中间成功一次即清零，不看历史累计），被禁用的条目自动退出订阅 |
| 子仓递归探活 | 多仓的 urls[] 逐个探活（可选，默认关） |
| 订阅输出 | `/sub`（多仓 JSON，**多仓自动平铺**：多仓源内的单仓全部展开，单仓源直接并入，按 URL 去重，避免多仓套多仓）、`?format=fan`（饭太硬风格）、`/sub/txt`（纯文本，同样平铺；带 `?name=` 时每行输出 `名称,URL`）、`?name=前缀`；`/sub/all` 输出父源原始列表（需 Token） |
| 所有仓源 | 订阅实际下发的「平铺层」单独可管：按父源/URL 对账生成，ID 稳定；可改名、改组、备注、排序、启用/禁用、单条或批量探活、删除（删除后父源再同步也不复活）；隐藏的条目不进订阅 |
| 备份 | 导出/导入（merge 合并 / replace 替换），Token 永不导出 |
| 安全 | 后台 Token 鉴权（X-Token / Bearer / ?token=，常数时间比较）、`/api/preview?url=` 内网地址黑名单（SSRF 防护）、请求体 10MB / 上传 2MB 限制 |

### 关于「未知类型」源

还没探活成功、或解析失败的源，类型是 `unknown`。此时分不清单仓还是多仓，**不会生成站点源、也不进订阅**——
把多仓地址当成单仓发给客户端，客户端拿到那一条会直接解析失败。首次探活确认类型后自动生成，无需手工操作；
新增/批量导入后默认会立刻后台探活一次，所以正常流程里几乎看不到这个中间态。

### 真实源兼容性（实测社区源得出的数据格式经验）

- **中文国际域名**：`肥猫.live` 自动 IDN→Punycode；无协议前缀自动补 `http://`
- **宽松 JSON**：`//` 注释、`/* */` 块注释、尾逗号、单引号字符串、BOM、正文前的公告杂文字——词法安全剥离后仍可解析（字符串字面量里的 `//` 不会被误剥）
- **多仓 urls 兼容**：对象 `{url,name}` 与纯字符串混排；`csp_XPathhttp://...` 类格式前缀自动剥离；空地址跳过
- **加密源**：`$#` 混淆壳等加密内容明确报「响应非 JSON 格式」，不会误判为可用

## 探活准确率的两个关键默认值

1. **UA 固定为 `okhttp/4.9.1`**：大量 TVBox 源站对浏览器 UA 返回 403，模拟客户端 UA 才能拿到真实结果（当前版本不支持自定义 UA）。
2. **默认跳过 TLS 证书校验**：大量三方源用自签/过期证书的 HTTPS，关闭校验才能判断「源本身是否可用」而不是被证书问题误判。若部署在受控内网可在设置页关闭（请知悉风险）。

## API 摘要

统一信封 `{"code":0,"msg":"ok","data":...}`；除 `/healthz` 与订阅输出外，`/api/*` 均需 Token。

| 方法 | 路径 | 说明 |
|---|---|---|
| GET/POST | `/api/sources` | 列表（keyword/type/status/group/enabled/page/sort 筛选）/ 新增（autoCheck 默认异步探活） |
| POST | `/api/sources/batch` | 批量新增，`onDuplicate: skip(默认)/update`，`autoCheck` 默认 true（导入后立即后台探活） |
| POST | `/api/sources/import` | 文件导入（multipart 字段 file，.json/.txt ≤2MB） |
| GET/PUT/DELETE | `/api/sources/{id}` | 详情 / 修改（改地址会重置类型与解析结果，且与已有源地址重复时返回 409） / 删除 |
| POST | `/api/sources/{id}/toggle` · `/{id}/check` | 切换启用 / 单源探活 |
| POST | `/api/sources/batch-action` | 批量 enable/disable/delete/group |
| GET | `/api/sources/{id}/preview` · `/api/preview?url=` | 实时预览（后者有 SSRF 内网黑名单） |
| POST/GET | `/api/check/run` · `/api/check/status` | 全量探活（父源 + 站点源，运行中 409）· 进度 |
| GET | `/api/stats` · `/api/groups` | 概览统计（父源口径 + `flat*` 站点源口径；`checking`/`flatChecking` 为检测中、不计入 `unknown`；`groupDist` 为一次算全的分组分布，前端不必再拉全表自己数）· 分组列表 |
| GET | `/api/flats` | 站点源列表（keyword/status/group/enabled/page/size/sort_by/sort_dir，单页上限 500） |
| PUT/DELETE | `/api/flats/{id}` | 改名/改组/备注/排序 · 删除（置隐藏，父源再同步不复活） |
| POST | `/api/flats/{id}/toggle` · `/{id}/check` | 切换启用 / 单条探活 |
| POST | `/api/flats/batch-action` · `/api/flats/batch-check` | 批量 enable/disable/delete/group · 批量探活（`{ids:[...]}` 或 `{all:true, keyword?...}`，并发受限 + 攒批落盘） |
| GET | `/api/flats/groups` | 站点源分组列表 |
| GET/PUT | `/api/config` | 配置读写（Token 脱敏 `**`，令牌仅支持配置文件修改） |
| GET/POST | `/api/export` · `/api/import/config` | 备份导出 / 导入 |
| GET | `/healthz` | 存活探针 |
| GET | `/sub` | 订阅（免鉴权，多仓自动平铺为单仓）：`?name=前缀`、`?format=fan`；`/sub/txt` 纯文本（带 `?name=` 时每行 `名称,URL`，可加前缀 `?name=客厅`）；`/sub/all` 输出父源原始列表（需 Token） |

## 环境变量（首次启动写入 config.json，之后以配置文件为准）

**优先级：环境变量（仅初次）→ config.json → 内置默认值。**

环境变量只在 `config.json` 里还没有该项时生效，用来给容器首次启动提供初始值；
一旦落盘，后续就以配置文件为准，重启不会被环境变量顶回去：

- **端口 / 订阅路径**：可在后台「设置」页改（写回 `config.json`），改完重启生效
- **令牌（TOKEN）**：页面不支持修改，直接改 `config.json` 的 `token` 字段后重启即可

需要让环境变量重新压过配置文件时，设置 `ENV_OVERRIDE=1`。

| 变量 | 默认 | 说明 |
|---|---|---|
| `TOKEN` | 随机生成 | 后台访问令牌（首启打印在启动日志，之后改 `config.json` 的 `token`） |
| `PORT` | 8080 | 监听端口（后台改后需重启生效；Docker 记得同步改 `ports` 映射） |
| `SUB_PATH` | /sub | 订阅路径（后台改后需重启生效） |
| `DATA_DIR` | /data（Docker）/ ./data | 数据目录（data.json + config.json） |
| `CHECK_INTERVAL` | 360 | 探活间隔（分钟） |
| `CHECK_TIMEOUT` | 15 | 单请求超时（秒） |
| `CHECK_CONCURRENCY` | 10 | 探活并发 |
| `STARTUP_CHECK` | false | 启动即全量探活 |
| `AUTO_DISABLE_FAILS` | 0 | 连续失败自动禁用阈值 |
| `INSECURE_TLS` | true | 跳过证书校验 |
| `CHECK_SUB_URLS` | false | 递归探活多仓子仓 |
| `ENV_OVERRIDE` | false | 设为 `1`/`true` 时环境变量强制覆盖配置文件 |
| `TZ` | Asia/Shanghai | 时区 |

想让环境变量重新生效的两种做法：

```bash
# 1) 临时强制覆盖
ENV_OVERRIDE=1 docker compose up -d

# 2) 干净重来：删掉配置里的对应键（或整个 config.json），下次启动按环境变量重新初始化
docker compose down && rm -rf ./data/config.json && docker compose up -d
```

启动日志会明确告诉你哪一项被忽略了：

```
[store] 环境变量 TOKEN, PORT, SUB_PATH 已设置，但 config.json 中已有对应配置，本次以配置文件为准（如需强制覆盖：ENV_OVERRIDE=1）
```


## 安全须知

- **订阅接口无鉴权是设计取舍**（客户端填不了 Token）。任何知道地址的人都能拉到订阅列表。
  公网部署请用反向代理加 IP 白名单 / Basic Auth，或仅内网暴露端口。
- `/api/preview?url=` 已内置 SSRF 防护：拒绝环回/内网/链路本地地址（127/8、10/8、172.16/12、192.168/16、169.254/16、::1、fc00::/7 等）。
  已入库源的探活/预览不做内网拦截——NAS 内网自建源属于正常使用场景。
- 启动横幅会打印访问令牌（方便首启登录），**分享日志前请先抹掉这一行**；`GET /api/config` 返回脱敏值 `**`。
- data.json 损坏时自动改名备份为 `data.json.corrupt.时间戳` 并以空数据启动，不覆盖不丢数据。

## 多架构构建（NAS 常为 arm64）

```bash
docker buildx build --platform linux/amd64,linux/arm64,linux/arm/v7 \
  -t yourname/tvbox-manager:latest --push .
```

本机没有 Docker 也能编译（宿主只需 Docker）：

```bash
docker run --rm -v "$PWD":/src -w /src -e CGO_ENABLED=0 \
  golang:1.27-alpine sh -c "go mod download && go build -ldflags='-s -w' -o bin/tvbox-manager ."
```

## 开发

```bash
go test ./...        # 4 个包全绿（parser / store / checker / api 端到端）
go build .
```

### 本地免打包调试（前端改完刷新即生效）

前端默认通过 `go:embed` 打包进二进制，所以直接 `go run .` 虽然不生成 exe，但**改 `web/` 后仍需重启进程**才生效。
用开发模式让静态资源直接从磁盘读，就能「改完存盘 → 浏览器刷新」：

```bash
go run . -web ./web          # 或 WEB_DIR=./web go run .
```

启动日志里会多出两行确认：

```
前端来源:  磁盘 D:\...\web
开发模式:  已开启，改 web/ 下文件后刷新浏览器生效（无需重新编译）
```

开发模式下静态资源返回 `Cache-Control: no-store`，浏览器不会缓存旧的 css/js。
访问 <http://127.0.0.1:8080/> 即可测页面，接口和订阅地址与生产行为完全一致
（`/sub`、`/healthz`、`/api/*` 都可用；日志里会打印访问令牌）。

只想换端口/数据目录时：

```bash
PORT=18080 DATA_DIR=./data go run . -web ./web
```

改后端 Go 代码则需重启 `go run`（可自行接 air/wgo 之类热重载工具）；不带 `-web` 时即为生产形态，前端取内嵌资源。

数据模型与模块划分见 `internal/`：`model`（结构体）/ `store`（JSON 持久化+原子写）/ `parser`（类型识别+归一化+宽松 JSON）/ `checker`（探活+错误分类+SSRF）/ `scheduler`（定时调度）/ `api`（路由+鉴权+订阅输出）。前端在 `web/`（原生 Vue3，无构建步骤，`go:embed` 内嵌）。

## 法律提示

本工具仅做**地址管理与可用性检测**，不存储、不转发、不解析任何视频内容。三方源的版权责任由源提供方承担。仅供个人学习与技术交流使用。
