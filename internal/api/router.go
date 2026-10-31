package api

import (
	"crypto/subtle"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"

	"tvbox-manager/internal/scheduler"
	"tvbox-manager/internal/store"
)

// API HTTP 路由与鉴权
type API struct {
	st   *store.Store
	sch  *scheduler.Scheduler
	ver  string
	web  fs.FS // 以 web 目录为根
	dev  bool  // 开发模式：禁缓存、前端从磁盘读
	reqN atomic.Int64

	// PastedDir 粘贴单仓配置的本地托管目录（main 注入 data/pasted；空 = 托管未启用）
	PastedDir string

	// 一键解析直播源的后台任务状态（同一时刻只允许一个，避免重复去打上游）
	resyncMu sync.Mutex
	resync   *liveResyncState
}

// maxPageSize 列表接口单页上限（sources 与 flats 统一，避免口径不一致）
const maxPageSize = 500

// New webFS 必须是「以 web 目录为根」的文件系统（prod: embed 子目录，dev: os.DirFS）。
func New(st *store.Store, sch *scheduler.Scheduler, version string, webFS fs.FS) *API {
	return &API{st: st, sch: sch, ver: version, web: webFS}
}

// DevMode 开发模式：前端资源不走缓存，便于改完刷新浏览器立即看到效果。
func (a *API) DevMode(on bool) *API {
	a.dev = on
	return a
}

// Router 组装全部路由（含静态资源）
func (a *API) Router() http.Handler {
	// /api/* 内层路由（整体套鉴权）
	apiMux := http.NewServeMux()

	// 源管理
	apiMux.HandleFunc("GET /api/sources", a.handleSourceList)
	apiMux.HandleFunc("POST /api/sources", a.handleSourceAdd)
	apiMux.HandleFunc("POST /api/sources/batch", a.handleSourceBatch)
	apiMux.HandleFunc("POST /api/sources/batch-action", a.handleBatchAction)
	apiMux.HandleFunc("POST /api/sources/import", a.handleSourceImport)
	apiMux.HandleFunc("POST /api/sources/import/json", a.handleSourceImportJSON)
	apiMux.HandleFunc("GET /api/sources/{id}", a.handleSourceGet)
	apiMux.HandleFunc("PUT /api/sources/{id}", a.handleSourceUpdate)
	apiMux.HandleFunc("DELETE /api/sources/{id}", a.handleSourceDelete)
	apiMux.HandleFunc("POST /api/sources/{id}/toggle", a.handleSourceToggle)
	apiMux.HandleFunc("POST /api/sources/{id}/restore", a.handleSourceRestore)
	apiMux.HandleFunc("GET /api/sources/{id}/preview", a.handlePreviewStored)
	apiMux.HandleFunc("GET /api/sources/{id}/config", a.handleSourceConfig)
	apiMux.HandleFunc("POST /api/sources/{id}/check", a.handleCheckOne)
	apiMux.HandleFunc("GET /api/groups", a.handleGroups)

	// 自动刮削（A-06）：手动收录 + 种子同步 + 候选确认，全部围绕源地址本身
	apiMux.HandleFunc("POST /api/scrape/import", a.handleScrapeImport)
	apiMux.HandleFunc("GET /api/scrape/seeds", a.handleScrapeSeedList)
	apiMux.HandleFunc("POST /api/scrape/seeds", a.handleScrapeSeedAdd)
	apiMux.HandleFunc("PUT /api/scrape/seeds/{id}", a.handleScrapeSeedUpdate)
	apiMux.HandleFunc("DELETE /api/scrape/seeds/{id}", a.handleScrapeSeedDelete)
	apiMux.HandleFunc("POST /api/scrape/sync", a.handleScrapeSync)
	apiMux.HandleFunc("GET /api/scrape/candidates", a.handleScrapeCandidates)
	apiMux.HandleFunc("POST /api/scrape/apply", a.handleScrapeApply)

	// 站点源（平铺层，订阅接口的数据来源）
	apiMux.HandleFunc("GET /api/flats", a.handleFlatList)
	apiMux.HandleFunc("GET /api/flats/groups", a.handleFlatGroups)
	apiMux.HandleFunc("PUT /api/flats/{id}", a.handleFlatUpdate)
	apiMux.HandleFunc("DELETE /api/flats/{id}", a.handleFlatDelete)
	apiMux.HandleFunc("POST /api/flats/{id}/toggle", a.handleFlatToggle)
	apiMux.HandleFunc("POST /api/flats/{id}/check", a.handleFlatCheck)
	apiMux.HandleFunc("POST /api/flats/batch-action", a.handleFlatBatchAction)
	apiMux.HandleFunc("POST /api/flats/batch-check", a.handleFlatCheckBatch)

	// 直播源（独立维护，/tv 直播页的频道来源）
	apiMux.HandleFunc("GET /api/live/sources", a.handleLiveSourceList)
	apiMux.HandleFunc("POST /api/live/sources", a.handleLiveSourceAdd)
	apiMux.HandleFunc("POST /api/live/sources/batch", a.handleLiveSourceBatchAdd)
	apiMux.HandleFunc("POST /api/live/sources/batch-action", a.handleLiveSourceBatchAction)
	apiMux.HandleFunc("POST /api/live/sources/batch-check", a.handleLiveSourceBatchCheck)
	apiMux.HandleFunc("GET /api/live/groups", a.handleLiveSourceGroups)
	apiMux.HandleFunc("POST /api/live/resync", a.handleLiveResync)
	apiMux.HandleFunc("GET /api/live/resync/status", a.handleLiveResyncStatus)
	apiMux.HandleFunc("PUT /api/live/sources/{id}", a.handleLiveSourceUpdate)
	apiMux.HandleFunc("DELETE /api/live/sources/{id}", a.handleLiveSourceDelete)
	apiMux.HandleFunc("POST /api/live/sources/{id}/toggle", a.handleLiveSourceToggle)
	apiMux.HandleFunc("POST /api/live/sources/{id}/check", a.handleLiveSourceCheck)

	// 预览与探活
	apiMux.HandleFunc("GET /api/preview", a.handlePreviewURL)
	apiMux.HandleFunc("POST /api/check/run", a.handleCheckRun)
	apiMux.HandleFunc("GET /api/check/status", a.handleCheckStatus)
	apiMux.HandleFunc("GET /api/stats", a.handleStats)

	// 配置与备份
	apiMux.HandleFunc("GET /api/config", a.handleConfigGet)
	apiMux.HandleFunc("PUT /api/config", a.handleConfigPut)
	apiMux.HandleFunc("GET /api/export", a.handleExport)
	apiMux.HandleFunc("POST /api/import/config", a.handleImport)

	// 外层路由
	root := http.NewServeMux()
	root.Handle("/api/", a.auth(apiMux))

	// 对外订阅（无鉴权，/all 除外——单独挂到鉴权树内）
	cfg := a.st.Config()
	sub := cfg.SubPath
	if sub == "" {
		sub = "/sub"
	}
	root.HandleFunc("GET "+sub, a.handleSub)
	root.HandleFunc("GET "+sub+"/txt", a.handleSubTxt)
	root.HandleFunc("GET "+sub+"/all", a.auth(http.HandlerFunc(a.handleSubAll)).ServeHTTP)

	root.HandleFunc("GET /healthz", a.handleHealthz)

	// 粘贴单仓的本地托管配置（无鉴权，与订阅一致）
	root.HandleFunc("GET /configs/{name}", a.handlePastedConfig)

	// /tv 看片页（内嵌 LibreTV 谱系前端）与配套接口：全部经 tvGuard
	//   1. 配置 showTv=false → 一律 404（菜单隐藏 + 路径失效，登录了也打不开）
	//   2. 设了令牌 → 必须已登录；页面请求跳登录页、接口请求返回 401
	//   3. 没设令牌 → 无鉴权模式，与 /api/* 同口径放行
	// 静态资源必须一起过守卫：否则拦了 HTML 却还能直接抓 /tv/js/app.js 与数据接口。
	root.Handle("/tv/", a.tvGuard(a.spaHandler()))
	root.Handle("GET /tv", a.tvGuard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/tv/", http.StatusTemporaryRedirect)
	})))
	// 聚合源下发 + CMS 接口代理 + 播放媒体代理（m3u8 改写/分片透传）
	root.Handle("GET /tvapi/sources", a.tvGuard(http.HandlerFunc(a.handleTVSources)))
	root.Handle("GET /tvapi/proxy", a.tvGuard(http.HandlerFunc(a.handleTVProxy)))
	root.Handle("GET /tvapi/media", a.tvGuard(http.HandlerFunc(a.handleTVMedia)))
	// 直播频道表：从平铺源 lives[] 聚合 m3u/txt 播放列表，解析合并后下发
	root.Handle("GET /tvapi/live", a.tvGuard(http.HandlerFunc(a.handleTVLive)))

	// 静态资源（前端 SPA，go:embed 注入）；不限定方法，避免与 /api/ 子树模式冲突
	root.Handle("/", a.spaHandler())

	root.HandleFunc("GET /api/version", a.auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ok(w, map[string]string{"version": a.ver})
	})).ServeHTTP)

	return root
}

// ---- 鉴权中间件 ----

// CookieName 登录态 Cookie 名。
// 后台是 SPA，令牌放在 localStorage，请求时用 X-Token 头；但浏览器打开 /tv/、
// 加载 /tv/js/*.js、video/hls 拉分片这类请求没法加自定义头，所以这一侧改用 Cookie。
// 名字与 localStorage 的键保持一致（tm_token），便于排查。
const CookieName = "tm_token"

// tokenFromRequest X-Token / Authorization Bearer / ?token= / Cookie
func tokenFromRequest(r *http.Request) string {
	if t := r.Header.Get("X-Token"); t != "" {
		return t
	}
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		if t := strings.TrimSpace(auth[7:]); t != "" {
			return t
		}
	}
	if t := r.URL.Query().Get("token"); t != "" {
		return t
	}
	// Cookie 里是 encodeURIComponent 后的值，含特殊字符时需还原
	if c, err := r.Cookie(CookieName); err == nil && c.Value != "" {
		if v, e := url.QueryUnescape(c.Value); e == nil {
			return v
		}
		return c.Value
	}
	return ""
}

// tokenMatch 常量时间比较，避免长度/前缀差异被时序侧信道利用
func tokenMatch(got, want string) bool {
	return len(got) > 0 && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// isPageRequest 是否「浏览器打开页面」的请求（决定未登录时跳登录页还是返回 401 JSON）。
// 接口与媒体分片用 Accept: */*，走 JSON 分支，避免播放器拿到一坨 HTML。
func isPageRequest(r *http.Request) bool {
	if r.Header.Get("Sec-Fetch-Mode") == "navigate" {
		return true
	}
	if strings.Contains(r.Header.Get("Accept"), "text/html") {
		return true
	}
	p := strings.ToLower(r.URL.Path)
	return strings.HasSuffix(p, ".html") || p == "/tv" || p == "/tv/"
}

// tvGuard /tv 看片页与 /tvapi/* 的统一守卫
func (a *API) tvGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cfg := a.st.Config()
		// 关闭 = 真关掉：直接 404，不区分「未登录」与「已登录」，
		// 避免把「这里有个看片页」这件事告诉外部扫描者。
		if !cfg.ShowTV {
			http.NotFound(w, r)
			return
		}
		// 未设令牌 = 无鉴权模式（与 /api/* 一致），此时没有"登录"概念，直接放行
		if cfg.Token == "" {
			next.ServeHTTP(w, r)
			return
		}
		if !tokenMatch(tokenFromRequest(r), cfg.Token) {
			if isPageRequest(r) {
				// 带上原始地址，登录后跳回来
				target := "/?login=1&next=" + url.QueryEscape(r.URL.RequestURI())
				http.Redirect(w, r, target, http.StatusFound)
				return
			}
			writeErr(w, http.StatusUnauthorized, 401, "unauthorized")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *API) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := a.st.Config().Token
		// 未设置令牌 = 无鉴权模式（首次部署/内网临时开放）：
		// /api/* 全部放行，前端会在登录页与设置页提示尽快设置令牌。
		// 设置令牌后下一个请求立即恢复校验，无需重启。
		if token == "" {
			next.ServeHTTP(w, r)
			return
		}
		if !tokenMatch(tokenFromRequest(r), token) {
			writeErr(w, http.StatusUnauthorized, 401, "unauthorized")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---- 统一信封 ----

// spaHandler 静态资源 + SPA 回退（无后缀路径找不到时回 index.html）。
// a.web 必须是「以 web 目录为根」的 FS：生产为 embed 子目录，开发模式为 os.DirFS("./web")。
func (a *API) spaHandler() http.Handler {
	sub := a.web
	fileServer := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.Trim(strings.TrimPrefix(r.URL.Path, "/"), "/")
		if p == "" {
			p = "index.html"
		}
		if _, err := fs.Stat(sub, p); err != nil {
			if strings.Contains(p, ".") { // 带后缀的按 404 处理
				http.NotFound(w, r)
				return
			}
			r.URL.Path = "/" // SPA 路由回退
		}
		// 缓存策略：开发模式彻底禁缓存（改完刷新即生效）；生产用 no-cache 强制协商，
		// 避免升级后浏览器仍用旧的 HTML/JS/CSS
		switch {
		case a.dev:
			w.Header().Set("Cache-Control", "no-store, must-revalidate")
			w.Header().Set("Pragma", "no-cache")
		case strings.HasSuffix(p, ".html") || strings.HasSuffix(p, ".js") || strings.HasSuffix(p, ".css"):
			w.Header().Set("Cache-Control", "no-cache, must-revalidate")
			w.Header().Set("Pragma", "no-cache")
		}
		fileServer.ServeHTTP(w, r)
	})
}

type envelope struct {
	Code int         `json:"code"`
	Msg  string      `json:"msg"`
	Data interface{} `json:"data,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func ok(w http.ResponseWriter, data interface{}) {
	writeJSON(w, http.StatusOK, envelope{Code: 0, Msg: "ok", Data: data})
}

func writeErr(w http.ResponseWriter, httpStatus, code int, msg string) {
	writeJSON(w, httpStatus, envelope{Code: code, Msg: msg})
}

func badRequest(w http.ResponseWriter, msg string) {
	writeErr(w, http.StatusBadRequest, 400, msg)
}

func notFound(w http.ResponseWriter, msg string) {
	writeErr(w, http.StatusNotFound, 404, msg)
}

func serverError(w http.ResponseWriter, err error) {
	writeJSON(w, http.StatusInternalServerError, envelope{Code: 500, Msg: err.Error()})
}

// maxJSONBody 请求体上限：粘贴导入的大 JSON 也走这里的接口，太小会直接报解析失败
const maxJSONBody = 20 << 20

// decodeJSON 读取请求体并限制 20MB
func decodeJSON(w http.ResponseWriter, r *http.Request, v interface{}) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil {
		badRequest(w, "请求体解析失败: "+err.Error())
		return false
	}
	return true
}
