package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"tvbox-manager/internal/model"
	"tvbox-manager/internal/parser"
	"tvbox-manager/internal/store"
)

// handleConfigGet 配置读取（Token 脱敏为 **）
func (a *API) handleConfigGet(w http.ResponseWriter, r *http.Request) {
	cfg := a.st.Config()
	masked := cfg
	masked.Token = "**"
	total := a.st.Count(store.Filter{})
	ok(w, map[string]interface{}{
		"config":       masked,
		"sourcesTotal": total,
	})
}

// handleConfigPut 修改配置，修改后 scheduler.Reload 立即生效
func (a *API) handleConfigPut(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		badRequest(w, "请求体读取失败: "+err.Error())
		return
	}
	var req model.Config
	if err := json.Unmarshal(raw, &req); err != nil {
		badRequest(w, "请求体解析失败: "+err.Error())
		return
	}
	// showTv 缺键视为「不改动」：不必让每个调用方都跟进新字段。
	// 否则按旧字段集 PUT 的脚本会因 bool 零值把看片页静默关掉。
	var probe map[string]json.RawMessage
	if json.Unmarshal(raw, &probe) == nil {
		if _, has := probe["showTv"]; !has {
			req.ShowTV = a.st.Config().ShowTV
		}
	}
	if req.CheckIntervalMinutes <= 0 {
		badRequest(w, "探活间隔必须大于 0 分钟")
		return
	}
	if req.CheckTimeoutSeconds <= 0 || req.CheckTimeoutSeconds > 120 {
		badRequest(w, "超时需在 1-120 秒之间")
		return
	}
	if req.CheckConcurrency <= 0 || req.CheckConcurrency > 100 {
		badRequest(w, "并发数需在 1-100 之间")
		return
	}
	if req.ScrapeIntervalHours < 0 || req.ScrapeIntervalHours > 8760 {
		badRequest(w, "刮削同步间隔需在 0-8760 小时之间（0 = 关闭）")
		return
	}
	if req.Port <= 0 || req.Port > 65535 {
		badRequest(w, "端口非法")
		return
	}
	if req.SubPath == "" || !strings.HasPrefix(req.SubPath, "/") {
		badRequest(w, "订阅路径必须以 / 开头")
		return
	}
	// 令牌：空 / "**"（脱敏回显值）表示保持不变；给了新值就做长度校验。
	// 鉴权中间件每次请求都读最新配置，这里改完下一个请求立即生效，无需重启。
	req.Token = strings.TrimSpace(req.Token)
	if t := req.Token; t != "" && t != "**" {
		if l := len(t); l < 6 || l > 64 {
			badRequest(w, "令牌长度需在 6-64 位之间")
			return
		}
	}
	if err := a.st.UpdateConfig(req); err != nil {
		serverError(w, err)
		return
	}
	cfg := a.st.Config()
	a.sch.Reload(&cfg)
	masked := cfg
	masked.Token = "**"
	ok(w, masked)
}

// handleExport 导出完整备份（Token 置空）
func (a *API) handleExport(w http.ResponseWriter, r *http.Request) {
	cfg := a.st.Config()
	cfg.Token = ""
	ok(w, map[string]interface{}{
		"version":    1,
		"exportedAt": time.Now().Format("2006-01-02 15:04:05"),
		"sources":    a.st.AllSources(),
		"config":     cfg,
	})
}

type importReq struct {
	Mode string `json:"mode"` // merge / replace
	Data struct {
		Sources []model.Source `json:"sources"`
		Config  *model.Config  `json:"config"`
	} `json:"data"`
}

// handleImport 导入备份
func (a *API) handleImport(w http.ResponseWriter, r *http.Request) {
	var req importReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if len(req.Data.Sources) == 0 && req.Data.Config == nil {
		badRequest(w, "备份内容为空")
		return
	}
	mode := req.Mode
	if mode != "replace" {
		mode = "merge"
	}

	// URL 归一化后再入库，保证去重键一致
	sources := make([]model.Source, 0, len(req.Data.Sources))
	for _, s := range req.Data.Sources {
		norm, err := parser.Normalize(s.URL)
		if err != nil {
			continue
		}
		s.URL = norm
		sources = append(sources, s)
	}

	var added, skipped, replaced int
	if mode == "replace" {
		if err := a.st.ReplaceAll(sources); err != nil {
			serverError(w, err)
			return
		}
		replaced = len(sources)
	} else {
		added, skipped = a.st.MergeAll(sources)
	}

	// 配置部分（备份中 Token 恒为空，保持现有令牌不变）
	if req.Data.Config != nil {
		in := *req.Data.Config
		in.Token = ""
		if err := a.st.UpdateConfig(in); err == nil {
			newCfg := a.st.Config()
			a.sch.Reload(&newCfg)
		}
	}

	ok(w, map[string]interface{}{
		"mode": mode, "added": added, "skipped": skipped, "replaced": replaced,
	})
}

func (a *API) handleHealthz(w http.ResponseWriter, r *http.Request) {
	cfg := a.st.Config()
	total := a.st.Count(store.Filter{})
	// tokenRequired 给登录页用：false = 未设置令牌（无鉴权模式），前端提示可直接登录并尽快设置
	// tvEnabled 给导航用：false = 已关闭看片页，菜单项不渲染（后端同时会把 /tv 全路径 404）
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":        "up",
		"sources":       total,
		"tokenRequired": cfg.Token != "",
		"tvEnabled":     cfg.ShowTV,
	})
}
