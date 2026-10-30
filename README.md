# tvbox-manager — TVBox 源维护系统

> **许可证**：[AGPL-3.0](LICENSE) —— 可自由使用、修改、分发，**商用也可，但必须开源**（含对外提供服务时公开源码）。
> 需要**闭源商用**请购买商业授权 → [COMMERCIAL.md](COMMERCIAL.md)。参与贡献请先阅读 [CONTRIBUTING.md](CONTRIBUTING.md)（含 CLA）。
> **联系**：QQ `1343314118` · 邮箱 `1343314118@qq.com`

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

`data/` 整个目录**不入库**（`.gitignore` 已忽略）：仓库里只有代码，`config.json` 含访问令牌、
`data.json` 是本机的源与检测记录，都不该进版本库。克隆后首次启动会自动创建 `data/` 目录及这两个文件，
无需任何手工准备。

或用 compose（默认从上面这个地址拉取镜像，不再本地构建）：

```bash
docker compose up -d
```

仓库里的 `docker-compose.yml` 用**命名卷** `tvbox-data`，首次挂载自动继承容器内属主，无需 chown。
数据在 `/var/lib/docker/volumes/<项目名>_tvbox-data/_data`，备份：

```bash
docker run --rm -v tvbox-data:/data -v $PWD:/backup alpine tar czf /backup/data.tgz /data
```

#### 改用相对路径挂载（数据留在项目目录）

只想把数据放在 `docker-compose.yml` 旁边时，把 `volumes` 换成绑定挂载：

```yaml
    volumes:
      - ./data:/data
```

并删掉文件末尾的 `volumes: tvbox-data: {}`。三个必须知道的点：

1. **`./` 前缀不能省**。写 `data:/data` 会被当成命名卷（数据跑到 `/var/lib/docker/volumes/data/_data`），`docker compose down -v` 会连数据一起删。
2. **相对路径只有 compose 认**。引擎层（`docker run -v`、`docker volume create --opt device=`）只接受绝对路径，`docker run -v ./data:/data` 直接报 `invalid characters for a local volume name`。
3. **基准是 compose 项目目录**，默认等于 compose 文件所在目录；用 `-f` 指向别处时基准会跟着变，可用 `--project-directory` 显式指定。

绑定挂载前必须先建目录改属主，否则容器内 app 用户写不进去，配合 `restart: unless-stopped` 就是无限重启：

```bash
mkdir -p ./data && chown -R 100:101 ./data
```

CentOS/RHEL 上 SELinux 处于 enforcing 时再加 `:z`（`- ./data:/data:z`）。

从命名卷迁到相对路径（`down` 不要带 `-v`）：

```bash
docker compose down
mkdir -p ./data
docker run --rm -v tvbox-data:/from -v "$PWD/data:/to" alpine \
  sh -c 'cp -a /from/. /to/ && chown -R 100:101 /to'
docker compose up -d
```

> 想要「由 Docker 管的命名卷、但落在项目目录」，可用 local driver + `${PWD}`：
> `driver: local` / `driver_opts: {type: none, o: bind, device: ${PWD}/data}`。
> `device` 同样不接受相对路径，必须靠 shell 把 `${PWD}` 展开。

要从源码构建镜像，用仓库根目录的 `Dockerfile`（多阶段，需能拉 `golang:1.27-alpine`）：
`docker build -t tvbox-manager:latest .`，然后把 compose 里的 `image` 换成 `tvbox-manager:latest` 并启用 `build: .`。

启动后：

- 管理后台：<http://服务器IP:8080/>（未设置 `TOKEN` 时为**无鉴权模式**：登录页可直接进入并提示尽快设置令牌；设置了则打印在启动日志的「访问令牌」一行）
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
| 源管理 | 增删改查、分组、排序、备注、启用/禁用（仅启用的进入订阅）；列表支持「↻ 刷新」强制重拉，异步探活过程中状态不会错位 |
| 批量录入 | 粘贴多行（`名称,URL` / `名称\|URL` / 纯 URL 混排）、.json / .txt 文件导入（前端预览勾选）；**导入后默认后台批量探活**（并发受限、攒批落盘），可用 `autoCheck:false` 关掉 |
| JSON 粘贴导入 | 粘贴整份配置，自动分流：单仓 → 存为本地源；多仓 → 只提取子仓地址；地址列表/备份 → 批量添加。支持 `dryRun` 先看预览（识别类型、条数、重复数）再落库 |
| 本地源托管 | 粘贴的单仓配置存到 `data/pasted/<id>.json`，由本服务对外提供地址，再自动建一条指向它的源，整条管线（探活→站点源→订阅）原样复用。编辑弹窗可直接改原文，见下方「本地源」 |
| 源类型识别 | 单仓（sites/lives/parses）/ 多仓（urls），多仓优先；结构摘要（站点数、直播数、解析数、spider、壁纸、站点名清单、子仓列表）；探活失败时可**手动定型**（强制单仓/多仓） |
| 实时预览 | 点一下就知道这个地址**现在**能不能用、里面有什么；原始 JSON 可复制/展开（全部复制在 http 环境下有三级兜底） |
| 探活 | 默认每 6 小时全量（可配），覆盖**父源 + 站点源**两轮（同地址只请求一次，直接复用结果），手动触发（运行中再触发返回 409），进度实时上报；失败原因分类：超时 / 域名无法解析 / 连接被拒绝 / 证书校验失败 / HTTP 状态码 / 响应非 JSON / 仓库结构不完整。本地源不参与检测，始终可用 |
| 自动禁用 | 连续失败 N 次自动禁用（默认关，0=不自动；**父源与站点源同口径**，中间成功一次即清零，不看历史累计），被禁用的条目自动退出订阅 |
| 子仓递归探活 | 多仓的 urls[] 逐个探活（可选，默认关） |
| 订阅输出 | `/sub`（多仓 JSON，**多仓自动平铺**：多仓源内的单仓全部展开，单仓源直接并入，按 URL 去重，避免多仓套多仓）、`?format=fan`（饭太硬风格）、`/sub/txt`（纯文本，同样平铺；带 `?name=` 时每行输出 `名称,URL`）、`?name=前缀`；`/sub/all` 输出父源原始列表（需 Token） |
| 所有仓源 | 订阅实际下发的「平铺层」单独可管：按父源/URL 对账生成，ID 稳定；可改名、改组、备注、排序、启用/禁用、单条或批量探活、删除（删除后父源再同步也不复活）；隐藏的条目不进订阅；支持按父源「恢复」重建被误删的条目 |
| 未进订阅排查 | 概览页「未进订阅」卡片直接筛出「启用了却一条可用站点源都没有」的源，并按成因细分（未定型 / 多仓无子仓），点进去即可就地处理 |
| 备份 | 导出/导入（merge 合并 / replace 替换），Token 永不导出 |
| 内嵌看片页 `/tv` | LibreTV 谱系前端（Apache-2.0）随二进制内嵌，与订阅同源免鉴权：平铺源里的苹果CMS接口自动聚合成搜索源（去重、100 上限），搜索/详情/播放全链路服务端代理（m3u8 清单改写 + 分片透传），浏览器零 CORS 问题 |
| 电视直播 `/tv/live.html` | 频道来自后台「直播源」页独立维护的播放列表（m3u/txt/json）：自动取自平铺源 `lives` 配置（探活时同步入库），也支持手动多行/表单添加、批量启停/移组/检测/删除；m3u 分组属性、txt `#genre#` 分组与多线路、GBK 编码自动识别；同名频道合并线路、分组按央视→卫视→地方→港澳台→平台直播排序；播放统一走媒体代理（HLS 改写 / FLV 长连透传 / `|User-Agent=` 防盗链头透传），断流自动重连、失败自动换线 |
| 安全 | 后台 Token 鉴权（X-Token / Bearer / ?token=，常数时间比较；**令牌未设置时为无鉴权模式**，界面会提示设置）、`/api/preview?url=` 内网地址黑名单（SSRF 防护）、请求体 20MB / 上传文件 20MB 限制 |

### 三种录入方式

| 方式 | 适用 |
|---|---|
| 文本框粘贴 | 一批 `名称,URL` 或纯 URL，最常用 |
| 文件导入 | `.json` / `.txt` 拖进来，前端先出预览再勾选入库 |
| JSON 内容粘贴 | 手里是**整份配置正文**（不是地址），比如从群里复制的单仓/多仓 JSON |

第三种会先判断内容是什么：单仓配置 → 存成本地源；多仓配置 → 只提取 `urls[]` 里的子仓地址（内容本身丢弃，因为多仓里嵌的是地址不是配置）；都不是 → 按地址列表解析。

### 本地源（粘贴单仓配置）

粘贴进来的配置正文本身没有 URL，而系统里所有能力（探活、站点源展开、订阅输出）都建立在 URL 上。托管就是把这件事补上：

```
粘贴内容 → data/pasted/<16位hex>.json
         → 生成源地址 http://<host>:<port>/configs/<16位hex>.json
         → 自动建一条指向该地址的源，类型记为 local
```

- **不参与探活**，状态恒为可用；站点源照常展开、进订阅，客户端拉订阅后拿到的是托管地址
- **编辑弹窗里能直接改原文**（宽弹窗、等宽字体、实时字符数/站点数提示）。保存前会走一遍宽松修复 + 解析校验，**必须能解析成单仓才落盘**，注释、裸换行、缺引号之类都会被自动修；文件名不变 → 托管地址不变 → 客户端不用改配置，重新拉一次就是新内容
- **改地址 = 脱离托管**：原托管文件删除且无法恢复，类型回到 `unknown`，要等下一轮探活定型（弹窗里有橙色提醒）
- **删除源会连带删除托管文件**
- `GET /configs/<name>` **无鉴权**（与订阅同级，客户端要能直接拉），文件名走 `^[a-f0-9]{16}\.json$` 白名单，杜绝路径穿越

只有本地源可编辑内容；其它源点编辑时没有「配置内容」区块，`GET /api/sources/{id}/config` 也会返回 400。

### 关于「未知类型」源

还没探活成功、或解析失败的源，类型是 `unknown`。此时分不清单仓还是多仓，**不会生成站点源、也不进订阅**——
把多仓地址当成单仓发给客户端，客户端拿到那一条会直接解析失败。首次探活确认类型后自动生成，无需手工操作；
新增/批量导入后默认会立刻后台探活一次，所以正常流程里几乎看不到这个中间态。

### 源列表有、仓源里没有？四种成因与补救

| 成因 | 现象 | 怎么处理 |
|---|---|---|
| 类型没定型 | `type=unknown`，`flatCount=0` | 编辑 → 类型 → **强制单仓/强制多仓**（手动定型优先于探活结果，之后探活成功也不会被覆盖） |
| 多仓但没解析出子仓 | `type=multi`，子仓列表为空 | 先点「预览」看 `urls` 到底有没有东西；确实是空壳就删掉，地址写错就改正 |
| 站点源被误删 | 仓源列表里曾经有，后来消失 | 该行点「恢复」，或筛选「未进订阅」后点「一键恢复本页」——按父源重建并**复活被删的墓碑** |
| 父源被禁用 | 源和仓源都在，但订阅里没有 | 重新启用；禁用的父源其站点源不会进订阅 |

「删除站点源」是打成**隐藏墓碑**而不是真删，正是为了让「恢复」能把用户误删的条目原样捡回来，同时保证父源再同步时不会自动复活。

不知道从哪下手时，回概览页点橙色「**未进订阅**」卡片：它只筛「已启用且一条可用站点源都没有」的源，
并告诉你里面多少条是「未定型」、多少条是「多仓无子仓」，两类处理方式不同。

### 真实源兼容性（实测社区源得出的数据格式经验）

- **中文国际域名**：`肥猫.live` 自动 IDN→Punycode；无协议前缀自动补 `http://`
- **宽松 JSON**：`//` 注释、`/* */` 块注释、尾逗号、单引号字符串、BOM、正文前的公告杂文字——词法安全剥离后仍可解析（字符串字面量里的 `//` 不会被误剥）
- **字符串内的裸控制字符**：`"name": "优` 换行 `酷"` 这类人肉编辑事故（严格 JSON 报 `Invalid control character`）会被识别并清理，`\t` 转空格
- **元素缺开引号**：`"其他剧",日韩剧",其他片",` 这种连片漏引号，按标准库报错位置逐处补齐（迭代修复，不是一遍式扫描，避免自身引号状态错乱）
- **超大 JSON**：单个配置文件十几 MB 很常见，请求体与上传上限都放到 20MB
- **多仓 urls 兼容**：对象 `{url,name}` 与纯字符串混排；`csp_XPathhttp://...` 类格式前缀自动剥离；空地址跳过
- **加密源**：`$#` 混淆壳等加密内容明确报「响应非 JSON 格式」，不会误判为可用

## 探活准确率的两个关键默认值

1. **UA 固定为 `okhttp/4.9.1`**：大量 TVBox 源站对浏览器 UA 返回 403，模拟客户端 UA 才能拿到真实结果（当前版本不支持自定义 UA）。
2. **默认跳过 TLS 证书校验**：大量三方源用自签/过期证书的 HTTPS，关闭校验才能判断「源本身是否可用」而不是被证书问题误判。若部署在受控内网可在设置页关闭（请知悉风险）。

## API 摘要

统一信封 `{"code":0,"msg":"ok","data":...}`；除 `/healthz`、订阅输出与 `/configs/*` 外，`/api/*` 均需 Token。

| 方法 | 路径 | 说明 |
|---|---|---|
| GET/POST | `/api/sources` | 列表（keyword/type/status/group/enabled/`notInSub=true`/page/sort 筛选；每行回填 `flatCount`）/ 新增（autoCheck 默认异步探活） |
| POST | `/api/sources/batch` | 批量新增，`onDuplicate: skip(默认)/update`，`autoCheck` 默认 true（导入后立即后台探活） |
| POST | `/api/sources/import` | 文件导入（multipart 字段 file，.json/.txt ≤20MB） |
| POST | `/api/sources/import/json` | 粘贴 JSON 导入：`{text,name,group,enabled,dryRun}`，`dryRun:true` 只出预览不落库 |
| GET/PUT/DELETE | `/api/sources/{id}` | 详情 / 修改（改地址会重置类型与解析结果，且与已有源地址重复时返回 409；`forceType` 手动定型；本地源可带 `content` 改托管原文） / 删除（连带清理托管文件） |
| POST | `/api/sources/{id}/toggle` · `/{id}/check` | 切换启用 / 单源探活 |
| POST | `/api/sources/{id}/restore` | 按父源重建站点源并复活被删条目，返回 `{restored,created,total,alive,hint}` |
| GET | `/api/sources/{id}/config` | 读本地源的托管配置原文（仅 `type=local`，其它返回 400） |
| POST | `/api/sources/batch-action` | 批量 enable/disable/delete/group |
| GET | `/api/sources/{id}/preview` · `/api/preview?url=` | 实时预览（后者有 SSRF 内网黑名单） |
| POST/GET | `/api/check/run` · `/api/check/status` | 全量探活（父源 + 站点源，运行中 409）· 进度 |
| GET | `/api/stats` · `/api/groups` | 概览统计（父源口径 + `flat*` 站点源口径；`checking`/`flatChecking` 为检测中、不计入 `unknown`；`notInSub` / `notInSubUnknown` / `notInSubEmpty` 为未进订阅及成因；`groupDist` 为一次算全的分组分布）· 分组列表 |
| GET | `/api/flats` | 站点源列表（keyword/status/group/enabled/page/size/sort_by/sort_dir，单页上限 500） |
| PUT/DELETE | `/api/flats/{id}` | 改名/改组/备注/排序 · 删除（置隐藏墓碑，父源再同步不复活） |
| POST | `/api/flats/{id}/toggle` · `/{id}/check` | 切换启用 / 单条探活 |
| POST | `/api/flats/batch-action` · `/api/flats/batch-check` | 批量 enable/disable/delete/group · 批量探活（`{ids:[...]}` 或 `{all:true, keyword?...}`，并发受限 + 攒批落盘） |
| GET | `/api/flats/groups` | 站点源分组列表 |
| GET/PUT | `/api/config` | 配置读写（Token 脱敏 `**`；令牌可在「设置」页修改，**立即生效**，也可改 `config.json` 后重启） |
| GET/POST | `/api/export` · `/api/import/config` | 备份导出 / 导入 |
| GET | `/healthz` | 存活探针 |
| GET | `/configs/{name}` | 本地源托管配置（无鉴权，文件名白名单校验） |
| GET | `/sub` | 订阅（免鉴权，多仓自动平铺为单仓）：`?name=前缀`、`?format=fan`；`/sub/txt` 纯文本（带 `?name=` 时每行 `名称,URL`）；`/sub/all` 输出父源原始列表（需 Token） |
| GET | `/tvapi/sources` | `/tv` 看片页搜索源聚合（免鉴权）：平铺源 CMS 接口去重下发，5 分钟缓存 |
| GET | `/tvapi/proxy?url=` | CMS 接口代理（免鉴权，SSRF 防护，10MB 上限） |
| GET | `/tvapi/media?url=` | 播放媒体代理：m3u8 清单改写 + 分片流式透传；地址可带 `\|User-Agent=..&Referer=..` 落为请求头 |
| GET/POST | `/api/live/sources` | 直播源列表（keyword/status/group/enabled/origin 筛选、分页）/ 单条添加（`{name,url,group,remark,check}`） |
| POST | `/api/live/sources/batch` | 多行批量添加：`{text,group,check,dryRun}`，`名称,地址` / `名称\|地址` / `名称 地址` / 纯地址混排；dryRun 返回逐行解析预览 |
| PUT/DELETE | `/api/live/sources/{id}` | 编辑（改名/改组后不再随父源同步，改地址清检测结论）/ 删除（自动同步条目留墓碑，父源再同步不复活） |
| POST | `/api/live/sources/{id}/toggle` · `/{id}/check` | 启停 · 检测（拉取列表 + 按分组抽样 20 个频道验证可用率，异步） |
| POST | `/api/live/sources/batch-action` · `/api/live/sources/batch-check` | 批量启用/禁用/移组/删除 · 批量检测（源间并发 3） |
| POST | `/api/live/resync` | 一键解析：重新拉取源列表各源的 `lives` 配置，提取直播地址同步进直播源表（后台任务，`{ids:[]}` 空 = 全部启用源；地址判重，已存在的只补关联） |
| GET | `/api/live/resync/status` | 一键解析进度：`{running,total,done,ok,failed,noLive,added,linked,detached,existed,errors}` |
| GET | `/api/live/groups` | 直播源分组列表 |
| GET | `/tvapi/live` | `/tv` 直播频道表（免鉴权）：聚合平铺源 `lives` 的播放列表，解析合并去重，10 分钟缓存 |

## 环境变量（首次启动写入 config.json，之后以配置文件为准）

**优先级：环境变量（仅初次）→ config.json → 内置默认值。**

环境变量只在 `config.json` 里还没有该项时生效，用来给容器首次启动提供初始值；
一旦落盘，后续就以配置文件为准，重启不会被环境变量顶回去：

- **端口 / 订阅路径**：可在后台「设置」页改（写回 `config.json`），改完重启生效
- **令牌（TOKEN）**：三种状态——
  1. **未设置**（首次启动没给 `TOKEN`，或 `config.json` 的 `token` 为空串）：无鉴权模式，登录页可直接进入并提示设置；
  2. **已设置**：启动日志打印「访问令牌」一行；
  3. **修改**：「设置 → 访问令牌」页改（6-64 位，**立即生效**，无需重启；改完当前会话自动切到新令牌，其他已登录浏览器需重新登录），或直接改 `config.json` 的 `token` 字段后重启（设为空串即回到无鉴权模式）。

需要让环境变量重新压过配置文件时，设置 `ENV_OVERRIDE=1`。

| 变量 | 默认 | 说明 |
|---|---|---|
| `TOKEN` | （空 = 无鉴权） | 后台访问令牌初值；不设置 = 无鉴权模式（登录页可直接进入并提示设置），之后可在「设置」页随时配置 |
| `PORT` | 8080 | 监听端口（后台改后需重启生效；Docker 记得同步改 `ports` 映射） |
| `SUB_PATH` | /sub | 订阅路径（后台改后需重启生效） |
| `DATA_DIR` | /data（Docker）/ ./data | 数据目录（`data.json` + `config.json` + `pasted/` 托管配置） |
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
- `/configs/<name>` 与订阅同为无鉴权（客户端要能直接拉配置）。文件名是随机 16 位 hex，只能靠猜；但**拿到 URL 就等于拿到内容**，
  公网部署时和数据目录一样按敏感资源对待。
- `/api/preview?url=` 已内置 SSRF 防护：拒绝环回/内网/链路本地地址（127/8、10/8、172.16/12、192.168/16、169.254/16、::1、fc00::/7 等）。
  已入库源的探活/预览不做内网拦截——NAS 内网自建源属于正常使用场景。
- 启动横幅会打印访问令牌（方便首启登录），**分享日志前请先抹掉这一行**；`GET /api/config` 返回脱敏值 `**`。
- data.json 损坏时自动改名备份为 `data.json.corrupt.时间戳` 并以空数据启动，不覆盖不丢数据。
- 上传与请求体上限统一 20MB（multipart 总量 64MB）。这是为十几 MB 的社区大仓库留的余量，反向代理若还有默认 1MB/10MB 限制需同步调大
  （nginx：`client_max_body_size 20m;`）。

## 多架构构建（NAS 常为 arm64）

推荐用脚本，一条命令搞定多架构构建 + 推送（在任意有 Docker 的机器上执行）：

```bash
./scripts/build-image.sh                    # 构建 amd64/arm64/armv7 并推送到默认仓库
TAG=1.4.7 ./scripts/build-image.sh          # 指定 tag（默认读 main.go 的 version）
./scripts/build-image.sh --load             # 只构建本机架构并载入 docker，用于本地验证
PLATFORMS=linux/amd64 ./scripts/build-image.sh   # 只构建单架构
```

脚本会同时打 `:版本号` 与 `:latest`，并默认关闭 attestation（部分个人版镜像仓库不接受
manifest list 里的 provenance 层，会报 `invalid manifest`）。

手工等价命令：

```bash
docker buildx build --platform linux/amd64,linux/arm64,linux/arm/v7 \
  -t registry.cn-hangzhou.aliyuncs.com/qdjzw/jiuseng:latest --push .
```

只想拿**二进制**、不需要镜像时，可在任意装了 Go 的机器上交叉编译（无需 Docker）：

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o tvbox-manager .
# NAS/树莓派：GOARCH=arm64（或 arm GOARM=7）
```

### 内存受限 / 拉不到 golang 镜像时（离线构建）

容器内跑 Go 编译需要几百 MB 内存，小内存 VPS（<512MB）会 swap 甚至被 OOM kill。
改成「本地交叉编译 + 只打运行层」，秒级完成，产物与多阶段构建一致：

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o dist/tvbox-manager .
docker build -f Dockerfile.offline -t registry.cn-hangzhou.aliyuncs.com/qdjzw/jiuseng:1.4.7 .
```

`Dockerfile.offline` 与根目录 `Dockerfile` 的最终阶段完全等价，只把「编译」这一步移到宿主侧，
因此**二进制必须用与 `go.mod` 相同版本的 Go 编译**，否则行为可能与预期不同。

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

数据模型与模块划分见 `internal/`：`model`（结构体）/ `store`（JSON 持久化+原子写+站点源对账）/ `parser`（类型识别+归一化+宽松 JSON）/ `checker`（探活+错误分类+SSRF）/ `scheduler`（定时调度）/ `api`（路由+鉴权+订阅输出+JSON 导入+托管配置）。前端在 `web/`（原生 Vue3，无构建步骤，`go:embed` 内嵌）。

## 变更记录

| 版本 | 要点 |
|---|---|
| 1.4.7 | 许可证由 Apache-2.0 改为 **AGPL-3.0 + 商业双授权**：商用闭源需购买授权，开源使用一律免费；新增 `COMMERCIAL.md`（授权边界与联系方式）、`CONTRIBUTING.md`（含 CLA，双授权合法性的前提）；README 顶部与末尾补充许可证说明 |
| 1.4.6 | 一键解析：直播源页 / 源列表页均可一键重新拉取各源 `lives` 配置并同步（源列表勾选则只解析选中项）；后台任务 + 实时进度，按地址判重（剥掉 `\|` 请求头参数后归一化），重复直接忽略、已删除的不复活；解析只覆盖 `lives`，不重跑站点探活 |
| 1.4.5 | 直播源独立维护页：手动添加（多行混排 / 单个表单）、批量启用/禁用/移组/检测/删除；自动同步条目完全可管（改名/改组后不被覆盖、删除留墓碑不复活）；双层检测（源级 + 按分组抽样 20 频道算可用率）；检测失效的源自动退出直播页；增删改启停即时生效 |
| 1.4.4 | `/tv` 新增**电视直播**：频道自动取自平铺源 `lives` 配置（m3u/txt/json 全格式、GBK 自动识别、`proxy://`/rtmp 自动剔除）；分组侧栏 + 频道搜索 + 多线路切换；HLS 复用媒体代理、FLV 用 mpegts.js（本地内嵌，无 CDN 依赖）；媒体代理支持 `|User-Agent=` 防盗链头并透传到清单每一跳；改长连超时（FLV 直播不再 5 分钟被掐断）；断流静默重连、失败自动换线 |
| 1.4.3 | 点播播放体验：先重试后报错（延迟确认制），不再「先提示失败再播出来」；`/tv/` 尾斜杠 404 修复 |
| 1.4.2 | 播放地址选组打分：`vod_play_url` 多组时自动选直链最多的一组，不再固定第一组 |
| 1.4.1 | 新增 `/tvapi/media` 播放媒体代理（m3u8 改写 + 分片透传），解决浏览器直连被 CORS/防盗链拦截 |
| 1.4.0 | 内嵌 LibreTV 谱系看片页 `/tv`（LBTV，Apache-2.0）：平铺源 CMS 接口自动聚合、搜索/详情/播放全代理，无需任何配置 |
| 1.3.10 | 本地源编辑三缺陷修复：读取失败红字+重试、内容防抖实时校验、绝不提交空内容 |
| 1.3.9 | 源列表「↻ 刷新」按钮 + 检测状态 2s 自愈轮询（异步探活时列表不再错位）；复制按钮三级兜底（http + IP 非安全上下文下也能复制） |
| 1.3.8 | 本地源可编辑托管 JSON 原文（校验不过拒绝保存，地址可变但会脱离托管） |
| 1.3.7 | 宽容解析补齐真实社区源：字符串内裸控制字符、元素缺开引号（迭代修复）；`fxz.json` / `4k.json` 这类配置可正常导入 |
| 1.3.6 | 「源列表有、仓源里没有」四件套：手动定型、按父源恢复（复活误删墓碑）、未进订阅量化与成因可视化；大 JSON（>10MB）读写上限提到 20MB |

## 许可证

本项目采用 **AGPL-3.0 + 商业授权** 双轨模式：

| 使用方式 | 是否付费 | 义务 |
|---|---|---|
| 个人自用、学习研究 | 免费 | 无 |
| 公司/组织内部部署，不对外提供服务 | 免费 | 无 |
| 修改后对外提供服务或分发 | 免费 | **必须公开完整对应源码**（AGPL §13，含网络服务场景） |
| 闭源集成、闭源 SaaS、白标/OEM、去掉标识转售 | **需商业授权** | 见 [COMMERCIAL.md](COMMERCIAL.md) |

一句话：**愿意开源就免费用，想闭源商用就买授权。**

为什么不用 MIT/Apache-2.0：它们允许任何人拿去闭源商用且无需回馈，本项目不希望出现"拿它做收费服务、一行不回馈"的情况，AGPL 是唯一能覆盖网络服务场景的强 copyleft 许可。

### 第三方组件

- `/tv` 看片页基于 [LibreTV](https://github.com/LibreSpark/LibreTV)（**Apache-2.0**），原始许可见 `web/tv/LICENSE`；Apache-2.0 与 AGPL-3.0 兼容，本项目整体按 AGPL-3.0 分发
- 前端 `web/tv/js/vendor/` 内的 hls.js、mpegts.js（Apache-2.0）与 Vue 3（MIT）遵循各自原许可证

## 联系与反馈

| 用途 | 方式 |
|---|---|
| 问题反馈 / 功能建议 | [GitHub Issues](https://github.com/jiuseng88/tvbox-manager/issues)（推荐，便于他人检索） |
| 即时沟通 | QQ **1343314118** |
| 邮件 / 商业授权 | **1343314118@qq.com** |

提交 Issue 时请附上版本号（后台概览页或 `./tvbox-manager -version`）、部署方式（Docker / 二进制）、
报错日志与复现步骤。涉及具体源地址的，请先确认该源可以公开讨论。

**商业授权**请在邮件标题注明 `[商业授权]`，并简要说明：是否闭源、部署规模、是否对外分发，见 [COMMERCIAL.md](COMMERCIAL.md)。

## 法律提示

本工具仅做**地址管理与可用性检测**，不存储、不转发、不解析任何视频内容。三方源的版权责任由源提供方承担。仅供个人学习与技术交流使用。
