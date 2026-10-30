package api

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"tvbox-manager/internal/model"
)

// 一键解析：重新从源列表拉取配置，提取 lives[] 同步进直播源表。
//
// 与探活解耦——目的只是拿直播地址，没必要重跑一遍站点解析（那要慢一个量级）。
// 同步执行会撞上反代 / CDN 的读超时（几十个源串起来一两分钟），所以做成后台任务，
// 前端轮询 /api/live/resync/status 展示进度。
//
// 地址判重交给存储层：liveKey（剥掉 | 请求头参数后归一化）相同的视为同一条，
// 已存在就只补父源关联，绝不重复插入；用户删过的墓碑条目也不会被复活。

const (
	liveResyncWorkers = 6  // 源之间并发：再多容易触发上游限流
	liveResyncDoor    = 15 // 单源拉取超时（秒）
)

// liveResyncIssue 单个源解析失败的原因
type liveResyncIssue struct {
	Name  string `json:"name"`
	Error string `json:"error"`
}

// liveResyncState 一键解析的进度与结果
type liveResyncState struct {
	Running    bool              `json:"running"`
	Total      int               `json:"total"`
	Done       int               `json:"done"`
	OK         int               `json:"ok"`        // 成功拉到并解析
	Failed     int               `json:"failed"`    // 拉取/解析失败
	NoLive     int               `json:"noLive"`    // 解析成功但没有 lives 配置
	LiveSites  int               `json:"liveSites"` // 解析出的直播地址总数（去重前）
	Added      int               `json:"added"`     // 新增到直播源表
	Linked     int               `json:"linked"`    // 已存在、补了父源关联
	Detached   int               `json:"detached"`  // 已存在、但父源里已没有，解除关联
	Existed    int               `json:"existed"`   // 已存在、判重忽略
	Errors     []liveResyncIssue `json:"errors"`
	StartedAt  time.Time         `json:"startedAt"`
	FinishedAt *time.Time        `json:"finishedAt,omitempty"`
}

type liveResyncReq struct {
	IDs []string `json:"ids"` // 指定源（源列表勾选项）；空 = 全部启用源
}

// handleLiveResync POST /api/live/resync 启动一键解析
func (a *API) handleLiveResync(w http.ResponseWriter, r *http.Request) {
	var req liveResyncReq
	if !decodeJSON(w, r, &req) {
		return
	}

	only := make(map[string]bool, len(req.IDs))
	for _, id := range req.IDs {
		if id != "" {
			only[id] = true
		}
	}
	targets := make([]model.Source, 0, 16)
	for _, s := range a.st.AllSources() {
		if s.Type == model.TypeLocal || strings.TrimSpace(s.URL) == "" {
			continue // 本地源是托管内容，拉不到也无需拉
		}
		if len(only) > 0 {
			if !only[s.ID] {
				continue
			}
		} else if !s.Enabled {
			continue
		}
		targets = append(targets, s)
	}
	if len(targets) == 0 {
		badRequest(w, "没有可解析的源（选中的源都是本地源，或没有已启用的源）")
		return
	}

	a.resyncMu.Lock()
	if a.resync != nil && a.resync.Running {
		st := *a.resync
		a.resyncMu.Unlock()
		ok(w, map[string]interface{}{"started": false, "state": st})
		return
	}
	state := &liveResyncState{
		Running:   true,
		Total:     len(targets),
		Errors:    []liveResyncIssue{},
		StartedAt: time.Now(),
	}
	a.resync = state
	a.resyncMu.Unlock()

	go a.runLiveResync(targets, state)
	ok(w, map[string]interface{}{"started": true, "state": *state})
}

// handleLiveResyncStatus GET /api/live/resync/status 查询进度
func (a *API) handleLiveResyncStatus(w http.ResponseWriter, r *http.Request) {
	a.resyncMu.Lock()
	st := a.resync
	var out liveResyncState
	if st != nil {
		out = *st
		if out.Errors == nil {
			out.Errors = []liveResyncIssue{}
		}
	}
	a.resyncMu.Unlock()
	ok(w, out)
}

// runLiveResync 后台执行：并发拉取 → 解析 → 逐源对账入库。
func (a *API) runLiveResync(targets []model.Source, state *liveResyncState) {
	ck := a.sch.Checker()
	sem := make(chan struct{}, liveResyncWorkers)
	var wg sync.WaitGroup

	for _, src := range targets {
		src := src
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()

			ctx, cancel := context.WithTimeout(context.Background(), liveResyncDoor*time.Second)
			res, _ := ck.FetchT(ctx, src.URL, false, liveResyncDoor)
			cancel()

			var (
				sites []model.LiveSiteEntry
				issue string
			)
			switch {
			case res == nil:
				issue = "拉取失败"
			case res.Status != model.StatusOK:
				issue = strings.TrimSpace(res.Msg)
				if issue == "" {
					issue = "拉取失败"
				}
			case res.Detail != nil:
				sites = res.Detail.LiveSites
			}

			a.resyncMu.Lock()
			defer a.resyncMu.Unlock()
			state.Done++
			if issue != "" {
				state.Failed++
				if len(state.Errors) < 30 {
					state.Errors = append(state.Errors, liveResyncIssue{Name: src.Name, Error: issue})
				}
				return
			}
			state.OK++
			state.LiveSites += len(sites)
			if len(sites) == 0 {
				state.NoLive++
				return
			}
			stat, _ := a.st.SyncLiveSites(src.ID, sites)
			state.Added += stat.Added
			state.Linked += stat.Linked
			state.Detached += stat.Detached
			state.Existed += stat.Existed
		}()
	}
	wg.Wait()

	now := time.Now()
	a.resyncMu.Lock()
	state.Running = false
	state.FinishedAt = &now
	a.resyncMu.Unlock()

	a.invalidateLiveCache()
}
