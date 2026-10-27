package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"tvbox-manager/internal/parser"
)

// setSubHeaders 订阅输出公共响应头
func setSubHeaders(w http.ResponseWriter, contentType string) {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "no-store")
}

// subItem 订阅输出条目
type subItem struct {
	URL  string `json:"url"`
	Name string `json:"name"`
	Type *int   `json:"type,omitempty"`
}

// handleSub 多仓订阅：数据来自「站点源」平铺层（多仓子仓 + 单仓自身，
// 由源列表自动同步生成），仅输出有效启用的条目（按 sort、name 排序）。
// ?name=xxx 名称加前缀；?format=fan 输出饭太硬风格（type:0）。
func (a *API) handleSub(w http.ResponseWriter, r *http.Request) {
	prefix := r.URL.Query().Get("name")
	fan := r.URL.Query().Get("format") == "fan"

	flats := a.st.EnabledFlats()
	urls := make([]subItem, 0, len(flats))
	for _, f := range flats {
		it := subItem{URL: f.URL, Name: f.Name}
		if it.Name == "" {
			it.Name = parser.HostOf(f.URL)
		}
		if prefix != "" {
			it.Name = prefix + "-" + it.Name
		}
		if fan {
			zero := 0
			it.Type = &zero
		}
		urls = append(urls, it)
	}
	setSubHeaders(w, "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"urls": urls})
}

// handleSubTxt 纯文本，默认每行一个 URL（兼容部分老客户端）。
// 带 ?name= 时每行输出「名称,URL」（名称带该前缀），否则纯文本里名称无处安放；
// 不传 name 时输出与以前完全一致，不影响现有客户端。
func (a *API) handleSubTxt(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	withName := q.Has("name")
	prefix := q.Get("name")

	flats := a.st.EnabledFlats()
	var sb strings.Builder
	for _, f := range flats {
		if withName {
			name := f.Name
			if name == "" {
				name = parser.HostOf(f.URL)
			}
			if prefix != "" {
				name = prefix + "-" + name
			}
			sb.WriteString(name)
			sb.WriteString(",")
		}
		sb.WriteString(f.URL)
		sb.WriteString("\n")
	}
	setSubHeaders(w, "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(sb.String()))
}

// handleSubAll 全量输出（含禁用，需 Token，排障用）——输出父源原始列表
func (a *API) handleSubAll(w http.ResponseWriter, r *http.Request) {
	type item struct {
		URL     string `json:"url"`
		Name    string `json:"name"`
		Enabled bool   `json:"enabled"`
		Status  string `json:"status"`
	}
	urls := make([]item, 0, 16)
	for _, s := range a.st.AllSources() {
		name := s.Name
		if name == "" {
			name = parser.HostOf(s.URL)
		}
		urls = append(urls, item{URL: s.URL, Name: name, Enabled: s.Enabled, Status: s.Status})
	}
	setSubHeaders(w, "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"urls": urls})
}
