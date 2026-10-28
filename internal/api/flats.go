package api

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"

	"tvbox-manager/internal/checker"
	"tvbox-manager/internal/model"
	"tvbox-manager/internal/store"
)

// ---- 站点源（平铺层）CRUD ----

func (a *API) handleFlatList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.FlatFilter{
		Keyword: q.Get("keyword"),
		Status:  q.Get("status"),
		Group:   q.Get("group"),
		Enabled: q.Get("enabled"),
		Sort:    q.Get("sort"),
		SortBy:  q.Get("sort_by"),
		SortDir: q.Get("sort_dir"),
	}
	fmt.Sscanf(q.Get("page"), "%d", &f.Page)
	fmt.Sscanf(q.Get("size"), "%d", &f.Size)
	if f.Size <= 0 {
		f.Size = 50
	}
	if f.Size > maxPageSize {
		f.Size = maxPageSize
	}
	if f.Page <= 0 {
		f.Page = 1
	}
	list, total := a.st.ListFlatsPage(f) // 一次快照拿到本页 + 总数，避免全量算两遍
	if list == nil {
		list = []model.FlatSource{}
	}
	ok(w, map[string]interface{}{
		"list":  list,
		"total": total,
		"page":  f.Page,
		"size":  f.Size,
	})
}

func (a *API) handleFlatGroups(w http.ResponseWriter, r *http.Request) {
	ok(w, a.st.FlatGroups())
}

type updateFlatReq struct {
	Name           *string `json:"name"`
	Group          *string `json:"group"`
	Remark         *string `json:"remark"`
	Sort           *int    `json:"sort"`
	TimeoutSeconds *int    `json:"timeoutSeconds"` // 本条单独的探活超时，0=用全局
}

func (a *API) handleFlatUpdate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, found := a.st.GetFlat(id); !found {
		notFound(w, "站点源不存在")
		return
	}
	var req updateFlatReq
	if !decodeJSON(w, r, &req) {
		return
	}
	err := a.st.UpdateFlat(id, func(fp *model.FlatSource) {
		if req.Name != nil {
			fp.Name = strings.TrimSpace(*req.Name)
			fp.NameAuto = false // 手动改名后不再随父源同步
		}
		if req.Group != nil {
			fp.Group = strings.TrimSpace(*req.Group)
			if fp.Group == "" {
				fp.Group = "默认"
			}
			fp.GroupAuto = false // 手动改组后不再随父源同步
		}
		if req.Remark != nil {
			fp.Remark = strings.TrimSpace(*req.Remark)
		}
		if req.Sort != nil {
			fp.Sort = *req.Sort
		}
		if req.TimeoutSeconds != nil {
			fp.TimeoutSeconds = *req.TimeoutSeconds
		}
	})
	if err != nil {
		serverError(w, err)
		return
	}
	updated, _ := a.st.GetFlat(id)
	ok(w, updated)
}

func (a *API) handleFlatDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	n, err := a.st.DeleteFlats([]string{id})
	if err != nil {
		serverError(w, err)
		return
	}
	if n == 0 {
		notFound(w, "站点源不存在")
		return
	}
	ok(w, map[string]interface{}{"deleted": id})
}

func (a *API) handleFlatToggle(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	err := a.st.UpdateFlat(id, func(fp *model.FlatSource) { fp.Enabled = !fp.Enabled })
	if err != nil {
		notFound(w, "站点源不存在")
		return
	}
	f, _ := a.st.GetFlat(id)
	ok(w, f)
}

// handleFlatCheck 单站点源探活（异步）
func (a *API) handleFlatCheck(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	f, found := a.st.GetFlat(id)
	if !found {
		notFound(w, "站点源不存在")
		return
	}
	a.st.UpdateFlatInMemory(id, func(fp *model.FlatSource) { fp.Status = model.StatusChecking })
	go func() {
		res, _ := a.sch.Checker().FetchT(context.Background(), f.URL, false, f.TimeoutSeconds)
		_ = a.st.UpdateFlatCheckResult(id, store.CheckResult{
			Status: res.Status, HTTPCode: res.HTTPCode, LatencyMs: res.LatencyMs,
			Msg: res.Msg, Type: res.Type, Detail: res.Detail,
		}, true)
	}()
	ok(w, map[string]interface{}{"id": id, "status": model.StatusChecking})
}

func (a *API) handleFlatBatchAction(w http.ResponseWriter, r *http.Request) {
	var req batchActionReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if len(req.IDs) == 0 {
		badRequest(w, "ids 不能为空")
		return
	}
	if len(req.IDs) > 2000 {
		badRequest(w, "单次最多 2000 条")
		return
	}
	n, err := a.st.BatchFlats(req.IDs, req.Action, strings.TrimSpace(req.Group))
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	ok(w, map[string]interface{}{"affected": n})
}

// checkBatchReq 站点源批量探活请求：传 ids 或 all=true（当前过滤结果）
type checkBatchReq struct {
	IDs     []string `json:"ids"`
	All     bool     `json:"all"`
	Keyword string   `json:"keyword"`
	Status  string   `json:"status"`
	Group   string   `json:"group"`
	Enabled string   `json:"enabled"`
}

// handleFlatCheckBatch 站点源批量探活（异步）。
// 之前前端「批量检测」是逐条打 /check，N 条 = N 个请求 + N 次全量落盘，
// 这里一次请求搞定：并发受限、内存回写、攒批落盘。
func (a *API) handleFlatCheckBatch(w http.ResponseWriter, r *http.Request) {
	var req checkBatchReq
	if !decodeJSON(w, r, &req) {
		return
	}

	var targets []model.FlatSource
	if req.All {
		targets = a.st.ListFlats(store.FlatFilter{
			Keyword: req.Keyword, Status: req.Status, Group: req.Group, Enabled: req.Enabled,
		})
	} else {
		if len(req.IDs) == 0 {
			badRequest(w, "ids 不能为空（或用 all=true 检测全部）")
			return
		}
		if len(req.IDs) > 2000 {
			badRequest(w, "单次最多 2000 条")
			return
		}
		seen := make(map[string]bool, len(req.IDs))
		for _, id := range req.IDs {
			if seen[id] {
				continue
			}
			seen[id] = true
			f, found := a.st.GetFlat(id)
			if !found || f.Hidden {
				continue
			}
			targets = append(targets, f)
		}
	}
	if len(targets) == 0 {
		badRequest(w, "没有可检测的站点源")
		return
	}

	for _, f := range targets {
		a.st.UpdateFlatInMemory(f.ID, func(fp *model.FlatSource) { fp.Status = model.StatusChecking })
	}
	go a.runFlatCheckBatch(targets)
	ok(w, map[string]interface{}{"count": len(targets), "status": model.StatusChecking})
}

func (a *API) runFlatCheckBatch(targets []model.FlatSource) {
	cfg := a.st.Config()
	conc := cfg.CheckConcurrency
	if conc <= 0 {
		conc = 5
	}
	ck := a.sch.Checker()
	sem := make(chan struct{}, conc)
	var wg sync.WaitGroup
	var mu sync.Mutex
	done := 0

	for _, t := range targets {
		t := t
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			defer func() {
				rec := recover()
				if rec == nil {
					return
				}
				log.Printf("[api] 站点源批量探活 panic（已隔离）: %v", rec)
				// 同异步探活：状态已置「检测中」，panic 后必须补一个终态
				func() {
					defer func() { _ = recover() }()
					_ = a.st.UpdateFlatCheckResult(t.ID, store.CheckResult{
						Status: model.StatusFail, Msg: "探活异常中断",
					}, false)
				}()
			}()
			res, _ := ck.FetchT(context.Background(), t.URL, false, t.TimeoutSeconds)
			if res == nil {
				res = &checker.Result{Status: model.StatusFail, Msg: "探活无结果"}
			}
			_ = a.st.UpdateFlatCheckResult(t.ID, store.CheckResult{
				Status: res.Status, HTTPCode: res.HTTPCode, LatencyMs: res.LatencyMs,
				Msg: res.Msg, Type: res.Type, Detail: res.Detail,
			}, false)

			mu.Lock()
			done++
			save := done%20 == 0
			mu.Unlock()
			if save {
				if err := a.st.Save(); err != nil {
					log.Printf("[api] 批量探活中途落盘失败: %v", err)
				}
			}
		}()
	}
	wg.Wait()
	if err := a.st.Save(); err != nil {
		log.Printf("[api] 批量探活落盘失败: %v", err)
	}
	log.Printf("[api] 站点源批量探活完成: %d 条", len(targets))
}
