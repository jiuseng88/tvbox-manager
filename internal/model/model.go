package model

import "time"

// 仓库类型
const (
	TypeSingle  = "single"  // 单仓：含 sites/lives/parses
	TypeMulti   = "multi"   // 多仓：含 urls 数组
	TypeFile    = "file"    // 由本地 JSON 文件导入
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
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	URL       string    `json:"url"`
	Type      string    `json:"type"`   // single / multi / file / unknown
	Group     string    `json:"group"`  // 分组，默认 "默认"
	Enabled   bool      `json:"enabled"` // 是否进入订阅输出，默认 true
	Remark    string    `json:"remark"`
	Sort      int       `json:"sort"`   // 订阅接口中的排序，小的在前

	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`

	// —— 探活结果 ——
	LastCheckAt *time.Time `json:"lastCheckAt,omitempty"`
	Status      string     `json:"status"` // ok / fail / unknown / checking
	HTTPCode    int        `json:"httpCode"`
	LatencyMs   int64      `json:"latencyMs"`
	CheckMsg    string     `json:"checkMsg"` // 失败原因或成功摘要
	FailStreak  int        `json:"failStreak"`
	OKStreak    int        `json:"okStreak"`
	LastOKAt    *time.Time `json:"lastOkAt,omitempty"`

	// —— 解析出的结构信息（探活/预览时回填）——
	Detail *Detail `json:"detail,omitempty"`
}

// Detail 仓库内容摘要
type Detail struct {
	Sites     int      `json:"sites"`
	Lives     int      `json:"lives"`
	Parses    int      `json:"parses"`
	SiteNames []string `json:"siteNames"` // 最多保留 50 个
	LiveNames []string `json:"liveNames"`
	Spider    string   `json:"spider"`    // jar 地址
	Wallpaper string   `json:"wallpaper"`
	SubURLs   []SubURL `json:"subUrls"`   // 多仓时的子仓列表
	RawSize   int      `json:"rawSize"`   // 原始 JSON 字节数
}

// SubURL 多仓中的子仓条目
type SubURL struct {
	Name   string `json:"name"`
	URL    string `json:"url"`
	Status string `json:"status,omitempty"` // 子仓单独探活结果（可选功能）
}

// Data 持久化根对象（data.json）
type Data struct {
	Version int      `json:"version"`
	Sources []Source `json:"sources"`
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
	UserAgent             string `json:"userAgent"`             // 探活 UA
	CheckSubURLs          bool   `json:"checkSubUrls"`          // 是否递归探活多仓子仓
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

// Stats 概览统计
type Stats struct {
	Total     int    `json:"total"`
	Enabled   int    `json:"enabled"`
	OK        int    `json:"ok"`
	Fail      int    `json:"fail"`
	Unknown   int    `json:"unknown"`
	Single    int    `json:"single"`
	Multi     int    `json:"multi"`
	Groups    int    `json:"groups"`
	LastRunAt string `json:"lastRunAt"`
	NextRunAt string `json:"nextRunAt"`
}

// DefaultConfig 内置默认值
func DefaultConfig() Config {
	return Config{
		Token:                "",
		Port:                 8080,
		SubPath:              "/sub",
		CheckIntervalMinutes: 360,
		CheckTimeoutSeconds:  15,
		CheckConcurrency:     10,
		StartupCheck:         false,
		AutoDisableAfterFails: 0,
		InsecureSkipVerify:   true,
		UserAgent:            "okhttp/4.9.1",
		CheckSubURLs:         false,
	}
}
