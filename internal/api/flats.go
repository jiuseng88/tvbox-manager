package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"

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
	if f.Size > 2000 {
		f.Size = 2000
	}
	if f.Page <= 0 {
		f.Page = 1
	}
	total := a.st.CountFlats(f)
	list := a.st.ListFlats(f)
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
	Name   *string `json:"name"`
	Group  *string `json:"group"`
	Remark *string `json:"remark"`
	Sort   *int    `json:"sort"`
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
	if n := a.st.DeleteFlats([]string{id}); n == 0 {
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
		res, _ := a.sch.Checker().Fetch(context.Background(), f.URL, false)
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
