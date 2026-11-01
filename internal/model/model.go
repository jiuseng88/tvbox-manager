package model

import "time"

// 仓库类型
const (
	TypeSingle  = "single" // 单仓：含 sites/lives/parses
	TypeMulti   = "multi"  // 多仓：含 urls 数组
	TypeFile    = "file"   // 由本地 JSON 文件导入
	TypeLocal   = "local"  // 本地源：粘贴托管的内容，不参与检测，始终可用
	TypeUnknown = "unknown"
)

// 探活状态
const (
	StatusUnknown  = "unknown"
	StatusOK       = "ok"
	StatusFail     = "fail"
	StatusChecking = "checking"
)

// Source 一条仓库源记录
type Source struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	URL     string `json:"url"`
	Type    string `json:"type"`    // single / multi / file / unknown
	Group   string `json:"group"`   // 分组，默认 "默认"
	Enabled bool   `json:"enabled"` // 是否进入订阅输出，默认 true
	Remark  string `json:"remark"`
	Sort    int    `json:"sort"` // 订阅接口中的排序，小的在前

	// 本条源单独的探活超时（秒），0 = 用全局 checkTimeoutSeconds。
	// 有的仓库文件很大，全局超时会把它误判成失效，这里可以单独放宽。
	TimeoutSeconds int `json:"timeoutSeconds,omitempty"`

	// ForceType 手动指定的类型（""=自动，single / multi）。探活成功也不会被覆盖：
	// 用于「确定这个地址是单仓，但源站探测一直失败」时强制生成站点源进订阅。
	ForceType string `json:"forceType,omitempty"`

	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`

	// —— 溯源（仅刮削导入的源有值，手工/托管源为空）——
	SeedID         string     `json:"seedId,omitempty"`        // 来自哪个刮削种子
	DiscoveredAt   *time.Time `json:"discoveredAt,omitempty"`  // 发现时间
	UpstreamFP     string     `json:"upstreamFp,omitempty"`    // 上游内容指纹（sha1）
	UpstreamRemoved bool      `json:"upstreamRemoved,omitempty"` // 上游已移除（漂移），仅标记不自动删除

	// —— 探活结果 ——
	LastCheckAt *time.Time `json:"lastCheckAt,omitempty"`
	Status      string     `json:"status"` // ok / fail / unknown / checking
	HTTPCode    int        `json:"httpCode"`
	LatencyMs   int64      `json:"latencyMs"`
	CheckMsg    string     `json:"checkMsg"` // 失败原因或成功摘要
	FailStreak  int        `json:"failStreak"`
	OKStreak    int        `json:"okStreak"`
	CheckCount  int        `json:"checkCount"` // 累计检测次数
	FailCount   int        `json:"failCount"`  // 累计检测不通次数
	LastOKAt    *time.Time `json:"lastOkAt,omitempty"`

	// —— 解析出的结构信息（探活/预览时回填）——
	Detail *Detail `json:"detail,omitempty"`

	// FlatCount 名下可用站点源条数（列表接口运行时回填，不落盘）。
	// 0 + 已启用 = 这条源不会出现在订阅里，前端据此显示原因并给「恢复」入口。
	FlatCount int `json:"flatCount"`
}

// Detail 仓库内容摘要
type Detail struct {
	Sites     int      `json:"sites"`
	Lives     int      `json:"lives"`
	Parses    int      `json:"parses"`
	SiteNames []string `json:"siteNames"` // 最多保留 50 个
	LiveNames []string `json:"liveNames"`
	Spider    string   `json:"spider"` // jar 地址
	Wallpaper string   `json:"wallpaper"`
	SubURLs   []SubURL `json:"subUrls"` // 多仓时的子仓列表
	RawSize   int      `json:"rawSize"` // 原始 JSON 字节数

	// CmsSites 可直接用于浏览器聚合搜索的苹果CMS站点接口（/tv 看片页的数据来源）。
	// 仅保留 type 支持 HTTP 直连的条目，单仓最多 100 条。
	CmsSites []SiteEntry `json:"cmsSites,omitempty"`

	// LiveSites 直播源地址（/tv 直播页的数据来源）：TVBox 配置 lives[] 里
	// 可直接 HTTP 拉取的 m3u/txt/json 播放列表。依赖 jar / proxy:// 的条目已剔除，
	// 单仓最多 50 条。
	LiveSites []LiveSiteEntry `json:"liveSites,omitempty"`
}

// LiveSiteEntry 一份直播播放列表：整份文件里包含若干分组与频道。
// 与 SiteEntry 不同，直播源不是「接口」，拉下来就是完整的频道表。
type LiveSiteEntry struct {
	Name  string `json:"name"`
	URL   string `json:"url"`
	Group string `json:"group,omitempty"` // TVBox 里 live 条目自带的分组标记
}

// SiteEntry 单个 CMS 站点接口条目（已归一化为「基础地址」形态，
// 前端会自行拼接 /api.php/provide/vod 路径）
type SiteEntry struct {
	Name  string `json:"name"`
	API   string `json:"api"`             // 基础地址（不含 /api.php 路径）
	Host  string `json:"host"`            // 展示与去重用
	Adult bool   `json:"adult,omitempty"` // 源配置标记的成人内容
}

// SubURL 多仓中的子仓条目
type SubURL struct {
	Name   string `json:"name"`
	URL    string `json:"url"`
	Status string `json:"status,omitempty"` // 子仓单独探活结果（可选功能）
}

// FlatSource 平铺站点源：由源列表自动展开生成（多仓拆为子仓、单仓并入自身），
// 订阅接口的数据源。同一 URL 出现在多个父源时共享同一条，ParentIDs 记录全部来源。
type FlatSource struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	URL       string   `json:"url"`              // 归一化后全局唯一
	ParentIDs []string `json:"parentIds"`        // 关联父源 ID
	Enabled   bool     `json:"enabled"`          // 自身开关；实际生效 = 自身 && 任一父源启用
	Hidden    bool     `json:"hidden,omitempty"` // 已删除（隐藏）：不参与列表/订阅，父源再次同步也不复活
	Group     string   `json:"group"`
	Remark    string   `json:"remark"`
	Sort      int      `json:"sort"`      // 创建时继承父源排序，可单独改
	NameAuto  bool     `json:"nameAuto"`  // 名称是否随父源同步（手动改名后置 false）
	GroupAuto bool     `json:"groupAuto"` // 分组是否随父源同步（手动改组后置 false）

	// 本条单独的探活超时（秒），创建时继承父源；0 = 用全局 checkTimeoutSeconds
	TimeoutSeconds int `json:"timeoutSeconds,omitempty"`

	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`

	// —— 探活结果（可单独检测）——
	LastCheckAt *time.Time `json:"lastCheckAt,omitempty"`
	Status      string     `json:"status"` // ok / fail / unknown / checking
	HTTPCode    int        `json:"httpCode"`
	LatencyMs   int64      `json:"latencyMs"`
	CheckMsg    string     `json:"checkMsg"`
	CheckCount  int        `json:"checkCount"` // 累计检测次数
	FailCount   int        `json:"failCount"`  // 累计检测不通次数
	FailStreak  int        `json:"failStreak"` // 连续失败次数（成功即清零，自动禁用看这个）
	OKStreak    int        `json:"okStreak"`   // 连续成功次数
	LastOKAt    *time.Time `json:"lastOkAt,omitempty"`

	// —— 运行时计算字段（每次列表查询重算，不作为持久化依据）——
	EffectiveEnabled bool     `json:"effectiveEnabled"` // 是否生效进入订阅
	ParentNames      []string `json:"parentNames,omitempty"`
}

// Data 持久化根对象（data.json）
type Data struct {
	Version     int          `json:"version"`
	Sources     []Source     `json:"sources"`
	Flats       []FlatSource `json:"flats,omitempty"`       // 平铺站点源（由 Sources 自动同步生成）
	LiveSources []LiveSource `json:"liveSources,omitempty"` // 直播源（auto 由 lives[] 同步 / manual 手动添加）

	// 自动刮削（A-06）：种子清单与候选区。刮削结果先入候选（pending），
	// 用户确认后才导入为源；不自动入库、不自动删除。
	ScrapeSeeds      []ScrapeSeed      `json:"scrapeSeeds,omitempty"`
	ScrapeCandidates []ScrapeCandidate `json:"scrapeCandidates,omitempty"`
}

// Config 系统配置（config.json），全部可通过环境变量覆盖初值
type Config struct {
	Token                 string `json:"token"`                 // 后台访问令牌
	Port                  int    `json:"port"`                  // 监听端口
	SubPath               string `json:"subPath"`               // 对外订阅路径
	CheckIntervalMinutes  int    `json:"checkIntervalMinutes"`  // 探活间隔，默认 360
	CheckTimeoutSeconds   int    `json:"checkTimeoutSeconds"`   // 单请求超时，默认 15
	CheckConcurrency      int    `json:"checkConcurrency"`      // 并发数，默认 10
	StartupCheck          bool   `json:"startupCheck"`          // 启动后是否立即探活，默认 false
	AutoDisableAfterFails int    `json:"autoDisableAfterFails"` // 连续失败 N 次自动禁用，0=不自动
	InsecureSkipVerify    bool   `json:"insecureSkipVerify"`    // 跳过 TLS 校验，默认 true
	CheckSubURLs          bool   `json:"checkSubUrls"`          // 是否递归探活多仓子仓
	// ShowTV 是否开放 /tv 看片页（含 /tv/ 静态资源与 /tvapi/*）。
	// 关闭后菜单项隐藏，且直接访问路径返回 404——不是"隐藏入口"，是真的关掉。
	// 注意零值为 false，默认值必须由 DefaultConfig 显式给 true，缺字段的老配置才能保持开启。
	ShowTV bool `json:"showTv"`
	// ScrapeIntervalHours 自动刮削定时同步间隔（小时）：0 = 关闭；>0 = 每隔 N 小时同步一次种子。
	// 定时同步与手动「同步全部种子」同一条链路：拉取 → 识别 → 候选区（不自动入库）。
	ScrapeIntervalHours int `json:"scrapeIntervalHours,omitempty"`
	// AllowLoopbackFetch 允许访问本机回环地址（127.0.0.1 / localhost / ::1）。
	// 默认关闭：SSRF 防护需要挡住「把本服务当跳板探内网」。但用户常把 TVBox 客户端自带的
	// 本地接口（如 http://127.0.0.1:9978/...）直接填成源，这类地址只在本机有意义，
	// 故提供一个显式开关。开启后也只放行回环，内网网段仍然拦截。
	AllowLoopbackFetch bool `json:"allowLoopbackFetch,omitempty"`
}

// CheckProgress 探活任务进度
type CheckProgress struct {
	Running   bool       `json:"running"`
	Total     int        `json:"total"`
	Done      int        `json:"done"`
	OK        int        `json:"ok"`
	Fail      int        `json:"fail"`
	StartedAt *time.Time `json:"startedAt,omitempty"`
	LastRunAt *time.Time `json:"lastRunAt,omitempty"`
	NextRunAt *time.Time `json:"nextRunAt,omitempty"`
	TriggerBy string     `json:"triggerBy"` // scheduler / manual
	Error     string     `json:"error,omitempty"`
}

// GroupCount 分组分布（概览用，按源数量降序）
type GroupCount struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// Stats 概览统计（Total/Enabled/OK... 为父源口径；Flat* 为订阅实际输出的站点源口径）
type Stats struct {
	Total     int    `json:"total"`
	Enabled   int    `json:"enabled"`
	OK        int    `json:"ok"`
	Fail      int    `json:"fail"`
	Checking  int    `json:"checking"` // 检测中（不算进 unknown）
	Unknown   int    `json:"unknown"`
	Single    int    `json:"single"`
	Multi     int    `json:"multi"`
	Groups    int    `json:"groups"`
	LastRunAt string `json:"lastRunAt"`
	NextRunAt string `json:"nextRunAt"`

	// 分组分布：由后端一次算全，前端不必再拉一份（分页）列表自己数
	GroupDist []GroupCount `json:"groupDist"`

	FlatTotal    int `json:"flatTotal"`
	FlatEnabled  int `json:"flatEnabled"`
	FlatOK       int `json:"flatOk"`
	FlatFail     int `json:"flatFail"`
	FlatChecking int `json:"flatChecking"`
	FlatUnknown  int `json:"flatUnknown"`

	// 「未进订阅」：启用了却没有任何可用站点源的源数量，按原因拆分。
	// 用户反馈过「源列表里有、仓源里没有」，这三个数就是那件事的量化入口。
	NotInSub        int `json:"notInSub"`
	NotInSubUnknown int `json:"notInSubUnknown"` // 原因：还没定型
	NotInSubEmpty   int `json:"notInSubEmpty"`   // 原因：多仓没解析出子仓地址
}

// DefaultConfig 内置默认值
func DefaultConfig() Config {
	return Config{
		Token:                "",
		Port:                 8080,
		SubPath:              "/sub",
		CheckIntervalMinutes: 360,
		CheckTimeoutSeconds:  10,
		CheckConcurrency:     10,
		StartupCheck:         false,
		// 连续失败自动禁用的默认阈值。曾经默认 0（关闭），大量用户从未进设置页
		// 打开过它，误以为功能坏了——现在默认给一个保守值：5 轮连续失败才禁用。
		// 已在 config.json 里显式保存过该键的不受影响（含主动填 0 关闭的）。
		AutoDisableAfterFails: 5, InsecureSkipVerify: true,
		CheckSubURLs: false,
		// /tv 看片页默认开放（保持升级前行为）；后台「设置」可关闭，关闭后菜单与路径一并失效
		ShowTV: true,
		// 本机回环默认不放行（SSRF 防护优先）；需要在「设置」显式打开才能用 127.0.0.1 的本地源
		AllowLoopbackFetch: false,
	}
}

// 直播源来源
const (
	LiveOriginAuto   = "auto"   // 由平铺源的 lives[] 自动同步
	LiveOriginManual = "manual" // 用户手动添加
)

// LiveSource 一条直播播放列表（m3u / txt / json）。
//
// 与 FlatSource 的区别：站点源是「一个个 CMS 接口」，直播源是「一份频道清单」，
// 拉下来会展开成成百上千个频道，所以检测指标也不同（频道数、分组数、抽样可用数）。
// auto 源随父源探活同步，manual 源完全独立；两者在同一个列表里统一管理。
type LiveSource struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	URL       string   `json:"url"` // 可带 |User-Agent=..&Referer=.. 请求头参数
	Group     string   `json:"group"`
	Remark    string   `json:"remark"`
	Sort      int      `json:"sort"`
	Enabled   bool     `json:"enabled"`
	Hidden    bool     `json:"hidden,omitempty"` // 已删除（墓碑）：auto 源父源再同步也不复活
	Origin    string   `json:"origin"`           // auto / manual
	ParentIDs []string `json:"parentIds,omitempty"`

	// NameAuto / GroupAuto：auto 源的名称/分组是否随父源同步。
	// 用户手动改过就置 false，后续同步不再覆盖（与站点源同口径）。
	NameAuto  bool `json:"nameAuto"`
	GroupAuto bool `json:"groupAuto"`

	// 本条单独的检测超时（秒），0 = 用全局 checkTimeoutSeconds
	TimeoutSeconds int `json:"timeoutSeconds,omitempty"`

	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`

	// —— 溯源（仅刮削导入的直播源有值）——
	SeedID         string     `json:"seedId,omitempty"`
	DiscoveredAt   *time.Time `json:"discoveredAt,omitempty"`
	UpstreamFP     string     `json:"upstreamFp,omitempty"`
	UpstreamRemoved bool      `json:"upstreamRemoved,omitempty"`

	// —— 检测结果 ——
	LastCheckAt *time.Time `json:"lastCheckAt,omitempty"`
	Status      string     `json:"status"` // ok / fail / unknown / checking
	HTTPCode    int        `json:"httpCode"`
	LatencyMs   int64      `json:"latencyMs"`
	CheckMsg    string     `json:"checkMsg"`
	CheckCount  int        `json:"checkCount"`
	FailCount   int        `json:"failCount"`
	FailStreak  int        `json:"failStreak"`
	OKStreak    int        `json:"okStreak"`
	LastOKAt    *time.Time `json:"lastOkAt,omitempty"`

	// —— 解析结果（检测时回填）——
	ChannelCount int `json:"channelCount"` // 解析出的频道数
	GroupCount   int `json:"groupCount"`   // 分组数
	AliveCount   int `json:"aliveCount"`   // 抽样中可播放的频道数
	SampleCount  int `json:"sampleCount"`  // 抽样总数（0 = 未做频道抽样）

	// —— 运行时字段（列表查询回填，不落盘）——
	ParentNames []string `json:"parentNames,omitempty"` // auto 源来自哪些父源（名称）
}

// ScrapeSeed 自动刮削种子：一条可定期同步的公开订阅源地址。
// 来源仅限用户主动添加（源集合站点 / 公开仓库直链），不做全网自动发现。
type ScrapeSeed struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	URL  string `json:"url"` // 归一化后唯一
	// Type 已废弃（Deprecated）：类型不再由用户选择，发现引擎按内容自动识别
	// （m3u 播放列表 → 直播源；含 sites 的仓库 → 源；多仓 urls[] 逐条归类）。
	// 字段仅保留以兼容旧数据与旧版 API，读写都不再影响任何分支。
	Type    string `json:"type,omitempty"`
	Group   string `json:"group"`
	Enabled bool   `json:"enabled"` // 是否参与同步
	Remark  string `json:"remark,omitempty"`
	// Depth 受控递归深度：1 = 仅展开种子自身的 urls[]（默认，兼容旧行为）；
	// 2 = 子仓若是多仓继续展开一层。0 视为 1。上限 2。
	Depth int `json:"depth,omitempty"`

	CreatedAt time.Time `json:"createdAt"`

	// —— 最近一次同步结果 ——
	LastSyncAt  *time.Time `json:"lastSyncAt,omitempty"`
	LastSyncMsg string     `json:"lastSyncMsg,omitempty"`
}

// ScrapeCandidate 刮削候选：同步后等待用户确认的条目。
// 语义：先候选后确认——候选不自动入库；apply 后按结果标记 applied/skipped。
//
// 稳定键：ID 由 (SeedID + 归一化URL) 派生，而非随机。这样同一来源+同一地址的候选
// 在多次同步间保持同一条记录，已确认状态（applied/skipped）不被整批覆盖，且能识别
// 「上游已移除」（drift）：本次同步发现集里不再出现、但此前已 applied 的候选会被标记
// upstreamStatus=removed，不自动删除，进入复核。
type ScrapeCandidate struct {
	ID       string `json:"id"` // 由 (SeedID + 归一化URL) 派生，稳定可重入
	SeedID   string `json:"seedId,omitempty"`
	SeedName string `json:"seedName,omitempty"`
	Name     string `json:"name"`
	URL      string `json:"url"` // 归一化后
	// Target 候选去向：source（导入源列表）/ live（导入直播源列表）。由发现引擎嗅探决定。
	Target  string `json:"target,omitempty"`
	Kind    string `json:"kind,omitempty"` // repo-single / repo-multi / urls / array / txt / playlist / live-json / live-txt
	Dup     bool   `json:"dup"`            // 已存在于现有源（同步时标记，不重复入库）
	Status  string `json:"status"`         // pending / applied / skipped
	FetchedAt time.Time `json:"fetchedAt"`

	// —— 重建后新增（均 omitempty，向后兼容旧数据）——
	DiscoveredDepth int        `json:"discoveredDepth,omitempty"` // 发现深度：0=种子自身内容，1+=递归层级
	ParentURL      string     `json:"parentUrl,omitempty"`       // 递归展开时的父地址（多仓子仓的上级）
	Origin         string     `json:"origin,omitempty"`          // 仓库内相对路径（如 live/iptv/cn.m3u），定位多层级目录来源
	Quality        int        `json:"quality,omitempty"`        // 质量分 0~100：可达/结构/丰富/新鲜，仅排序，不阻断导入
	Summary        string     `json:"summary,omitempty"`        // 富集摘要：如 "12 站点" / "800 频道"
	FP             string     `json:"fp,omitempty"`             // 上游内容指纹（sha1），用于内容级漂移
	UpstreamStatus string     `json:"upstreamStatus,omitempty"` // present / removed / ""（未比对）
	LastSeenAt     time.Time  `json:"lastSeenAt,omitempty"`     // 最近一次同步仍在发现集
	AppliedAt      *time.Time `json:"appliedAt,omitempty"`      // 被 apply 的时间
}
