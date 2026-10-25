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

// handleSub 多仓订阅：仅输出 enabled=true，按 sort、name 排序。
// ?name=xxx 名称加前缀；?format=fan 输出饭太硬风格（type:0）。
func (a *API) handleSub(w http.ResponseWriter, r *http.Request) {
	prefix := r.URL.Query().Get("name")
	fan := r.URL.Query().Get("format") == "fan"

	type item struct {
		URL  string `json:"url"`
		Name string `json:"name"`
		Type *int   `json:"type,omitempty"`
	}
	urls := make([]item, 0, 16)
	for _, s := range a.st.EnabledSources() {
		name := s.Name
		if name == "" {
			name = parser.HostOf(s.URL)
		}
		if prefix != "" {
			name = prefix + "-" + name
		}
		it := item{URL: s.URL, Name: name}
		if fan {
			zero := 0
			it.Type = &zero
		}
		urls = append(urls, it)
	}
	setSubHeaders(w, "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"urls": urls})
}

// handleSubTxt 纯文本每行一个 URL（兼容部分老客户端）
func (a *API) handleSubTxt(w http.ResponseWriter, r *http.Request) {
	var sb strings.Builder
	for _, s := range a.st.EnabledSources() {
		sb.WriteString(s.URL)
		sb.WriteString("\n")
	}
	setSubHeaders(w, "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(sb.String()))
}

// handleSubAll 全量输出（含禁用，需 Token，排障用）
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
