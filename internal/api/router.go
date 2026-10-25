package api

import (
	"crypto/subtle"
	"encoding/json"
	"io/fs"
	"net/http"
	"strings"
	"sync/atomic"

	"tvbox-manager/internal/scheduler"
	"tvbox-manager/internal/store"
)

// API HTTP 路由与鉴权
type API struct {
	st   *store.Store
	sch  *scheduler.Scheduler
	ver  string
	web  fs.FS
	reqN atomic.Int64
}

func New(st *store.Store, sch *scheduler.Scheduler, version string, webFS fs.FS) *API {
	return &API{st: st, sch: sch, ver: version, web: webFS}
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
	apiMux.HandleFunc("GET /api/sources/{id}", a.handleSourceGet)
	apiMux.HandleFunc("PUT /api/sources/{id}", a.handleSourceUpdate)
	apiMux.HandleFunc("DELETE /api/sources/{id}", a.handleSourceDelete)
	apiMux.HandleFunc("POST /api/sources/{id}/toggle", a.handleSourceToggle)
	apiMux.HandleFunc("GET /api/sources/{id}/preview", a.handlePreviewStored)
	apiMux.HandleFunc("POST /api/sources/{id}/check", a.handleCheckOne)
	apiMux.HandleFunc("GET /api/groups", a.handleGroups)

	// 预览与探活
	apiMux.HandleFunc("GET /api/preview", a.handlePreviewURL)
	apiMux.HandleFunc("POST /api/check/run", a.handleCheckRun)
	apiMux.HandleFunc("GET /api/check/status", a.handleCheckStatus)
	apiMux.HandleFunc("GET /api/stats", a.handleStats)

	// 配置与备份
	apiMux.HandleFunc("GET /api/config", a.handleConfigGet)
	apiMux.HandleFunc("PUT /api/config", a.handleConfigPut)
	apiMux.HandleFunc("POST /api/config/token", a.handleTokenChange)
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

	// 静态资源（前端 SPA，go:embed 注入）；不限定方法，避免与 /api/ 子树模式冲突
	root.Handle("/", a.spaHandler())

	root.HandleFunc("GET /api/version", a.auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ok(w, map[string]string{"version": a.ver})
	})).ServeHTTP)

	return root
}

// ---- 鉴权中间件 ----

// tokenFromRequest X-Token / Authorization Bearer / ?token=
func tokenFromRequest(r *http.Request) string {
	if t := r.Header.Get("X-Token"); t != "" {
		return t
	}
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimSpace(auth[7:])
	}
	return r.URL.Query().Get("token")
}

func (a *API) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := a.st.Config().Token
		got := tokenFromRequest(r)
		if token == "" || len(got) == 0 ||
			subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			writeErr(w, http.StatusUnauthorized, 401, "unauthorized")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---- 统一信封 ----

// spaHandler 静态资源 + SPA 回退（无后缀路径找不到时回 index.html）
func (a *API) spaHandler() http.Handler {
	sub, err := fs.Sub(a.web, "web")
	if err != nil {
		panic(err)
	}
	fileServer := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
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

// decodeJSON 读取请求体并限制 2MB
func decodeJSON(w http.ResponseWriter, r *http.Request, v interface{}) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil {
		badRequest(w, "请求体解析失败: "+err.Error())
		return false
	}
	return true
}
