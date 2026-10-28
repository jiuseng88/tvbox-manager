package api

import (
	"context"
	"net/http"
	"time"

	"tvbox-manager/internal/checker"
	"tvbox-manager/internal/model"
	"tvbox-manager/internal/store"
)

// previewPayload 预览响应（doc 6.2）
type previewPayload struct {
	ID        string        `json:"id"`
	Name      string        `json:"name"`
	URL       string        `json:"url"`
	FinalURL  string        `json:"finalUrl"`
	Type      string        `json:"type"`
	HTTPCode  int           `json:"httpCode"`
	LatencyMs int64         `json:"latencyMs"`
	Size      int           `json:"size"`
	FetchedAt time.Time     `json:"fetchedAt"`
	Detail    *model.Detail `json:"detail"`
	Raw       string        `json:"raw"`
	Truncated bool          `json:"truncated"`
	ErrMsg    string        `json:"errMsg,omitempty"`
}

const rawLimit = 100 * 1024

func buildPreview(id, name, url string, res *checker.Result, body []byte) previewPayload {
	p := previewPayload{
		ID: id, Name: name, URL: url,
		FinalURL:  res.FinalURL,
		Type:      res.Type,
		HTTPCode:  res.HTTPCode,
		LatencyMs: res.LatencyMs,
		Size:      res.Size,
		FetchedAt: time.Now(),
		Detail:    res.Detail,
	}
	if res.Status != model.StatusOK {
		p.ErrMsg = res.Msg
	}
	raw := string(body)
	if len(raw) > rawLimit {
		p.Raw = raw[:rawLimit]
		p.Truncated = true
	} else {
		p.Raw = raw
	}
	return p
}

// handlePreviewStored 已入库源的实时预览（已录入源不做内网拦截，NAS 内网源属正常场景）
func (a *API) handlePreviewStored(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	src, found := a.st.Get(id)
	if !found {
		notFound(w, "源不存在")
		return
	}
	res, body := a.sch.Checker().Fetch(r.Context(), src.URL, false)
	ok(w, buildPreview(src.ID, src.Name, src.URL, res, body))
}

// handlePreviewURL 未入库地址直接预览（添加表单用），SSRF 防护强制开启
func (a *API) handlePreviewURL(w http.ResponseWriter, r *http.Request) {
	rawURL := r.URL.Query().Get("url")
	if rawURL == "" {
		badRequest(w, "缺少 url 参数")
		return
	}
	res, body := a.sch.Checker().Fetch(r.Context(), rawURL, true)
	ok(w, buildPreview("", "", rawURL, res, body))
}

// handleCheckOne 单源探活（异步）
func (a *API) handleCheckOne(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	src, found := a.st.Get(id)
	if !found {
		notFound(w, "源不存在")
		return
	}
	a.asyncCheckOne(src.ID, src.URL, src.TimeoutSeconds)
	ok(w, map[string]interface{}{"id": id, "status": model.StatusChecking})
}

func (a *API) asyncCheckOne(id, url string, timeoutSeconds int) {
	a.st.UpdateInMemory(id, func(sp *model.Source) {
		sp.Status = model.StatusChecking
	})
	go func() {
		res, _ := a.sch.Checker().FetchT(context.Background(), url, false, timeoutSeconds)
		_ = a.st.UpdateCheckResult(id, store.CheckResult{
			Status: res.Status, HTTPCode: res.HTTPCode, LatencyMs: res.LatencyMs,
			Msg: res.Msg, Type: res.Type, Detail: res.Detail,
		}, true)
	}()
}

// handleCheckRun 手动全量探活
func (a *API) handleCheckRun(w http.ResponseWriter, r *http.Request) {
	if err := a.sch.RunManual(); err != nil {
		writeJSON(w, http.StatusConflict, envelope{Code: 409, Msg: err.Error(), Data: a.sch.Progress()})
		return
	}
	ok(w, a.sch.Progress())
}

// handleCheckStatus 探活进度
func (a *API) handleCheckStatus(w http.ResponseWriter, r *http.Request) {
	ok(w, a.sch.Progress())
}

// handleStats 概览统计
func (a *API) handleStats(w http.ResponseWriter, r *http.Request) {
	prog := a.sch.Progress()
	lastRun, nextRun := "", ""
	if prog.LastRunAt != nil {
		lastRun = prog.LastRunAt.Format("2006-01-02 15:04:05")
	}
	if prog.NextRunAt != nil {
		nextRun = prog.NextRunAt.Format("2006-01-02 15:04:05")
	}
	ok(w, a.st.Stats(lastRun, nextRun))
}
