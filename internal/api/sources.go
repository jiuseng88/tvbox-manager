package api

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"

	"tvbox-manager/internal/checker"
	"tvbox-manager/internal/model"
	"tvbox-manager/internal/parser"
	"tvbox-manager/internal/store"
)

// ---- 源 CRUD ----

func (a *API) handleSourceList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.Filter{
		Keyword: q.Get("keyword"),
		Type:    q.Get("type"),
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
	total := a.st.Count(f)
	list := a.st.List(f)
	if list == nil {
		list = []model.Source{}
	}
	ok(w, map[string]interface{}{
		"list":  list,
		"total": total,
		"page":  f.Page,
		"size":  f.Size,
	})
}

type addSourceReq struct {
	Name           string `json:"name"`
	URL            string `json:"url"`
	Group          string `json:"group"`
	Remark         string `json:"remark"`
	Enabled        *bool  `json:"enabled"`
	AutoCheck      *bool  `json:"autoCheck"`
	TimeoutSeconds *int   `json:"timeoutSeconds"` // 本条单独的探活超时，0=用全局
}

func (a *API) handleSourceAdd(w http.ResponseWriter, r *http.Request) {
	var req addSourceReq
	if !decodeJSON(w, r, &req) {
		return
	}
	norm, err := parser.Normalize(req.URL)
	if err != nil {
		badRequest(w, "地址非法: "+err.Error())
		return
	}
	if _, exists := a.st.GetByURL(norm); exists {
		writeErr(w, http.StatusConflict, 409, "该地址已存在")
		return
	}
	src := model.Source{
		Name:   strings.TrimSpace(req.Name),
		URL:    norm,
		Group:  strings.TrimSpace(req.Group),
		Remark: strings.TrimSpace(req.Remark),
		Type:   model.TypeUnknown,
		Status: model.StatusUnknown,
	}
	if req.TimeoutSeconds != nil {
		src.TimeoutSeconds = *req.TimeoutSeconds
	}
	if src.Name == "" {
		src.Name = parser.HostOf(norm)
	}
	if src.Group == "" {
		src.Group = "默认"
	}
	if req.Enabled != nil {
		src.Enabled = *req.Enabled
	} else {
		src.Enabled = true
	}
	if err := a.st.Add(&src); err != nil {
		serverError(w, err)
		return
	}
	autoCheck := true
	if req.AutoCheck != nil {
		autoCheck = *req.AutoCheck
	}
	if autoCheck {
		a.asyncCheckOne(src.ID, src.URL, src.TimeoutSeconds)
	}
	created, _ := a.st.Get(src.ID)
	ok(w, created)
}

// batchItem 结构化条目：名称里带逗号/竖线时不会被 splitNameURL 切坏
type batchItem struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

type batchAddReq struct {
	URLs        []string    `json:"urls"`
	Items       []batchItem `json:"items"` // 结构化条目，优先于 URLs
	Group       string      `json:"group"`
	Enabled     *bool       `json:"enabled"`
	AutoCheck   *bool       `json:"autoCheck"`   // 默认 true：新增的源立即探活一次，类型定型后才会进订阅
	OnDuplicate string      `json:"onDuplicate"` // skip(默认) / update
}

// entries 把两种入参统一成结构化条目
func (req batchAddReq) entries() []batchItem {
	if len(req.Items) > 0 {
		out := make([]batchItem, 0, len(req.Items))
		for _, it := range req.Items {
			u := strings.TrimSpace(it.URL)
			if u == "" {
				continue
			}
			out = append(out, batchItem{Name: strings.TrimSpace(it.Name), URL: u})
		}
		return out
	}
	out := make([]batchItem, 0, len(req.URLs))
	for _, raw := range req.URLs {
		n, u := splitNameURL(raw)
		out = append(out, batchItem{Name: n, URL: u})
	}
	return out
}

func (a *API) handleSourceBatch(w http.ResponseWriter, r *http.Request) {
	var req batchAddReq
	if !decodeJSON(w, r, &req) {
		return
	}
	a.batchAdd(w, req)
}

func (a *API) batchAdd(w http.ResponseWriter, req batchAddReq) {
	list := req.entries()
	if len(list) == 0 {
		badRequest(w, "urls 不能为空")
		return
	}
	if len(list) > 2000 {
		badRequest(w, "单次最多 2000 条")
		return
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	group := strings.TrimSpace(req.Group)
	if group == "" {
		group = "默认"
	}

	type skippedItem struct {
		URL    string `json:"url"`
		Reason string `json:"reason"`
	}
	var skipped []skippedItem
	var ids []string
	var added []model.Source // 新入库的源：用于异步探活（确定类型后才会生成站点源）
	addedCount := 0
	updated := 0

	for _, it := range list {
		name, url := it.Name, it.URL
		norm, err := parser.Normalize(url)
		if err != nil {
			skipped = append(skipped, skippedItem{URL: url, Reason: "地址非法: " + err.Error()})
			continue
		}
		exist, found := a.st.GetByURL(norm)
		if found {
			if req.OnDuplicate == "update" {
				_ = a.st.Update(exist.ID, func(sp *model.Source) {
					if name != "" {
						sp.Name = name
					}
					sp.Group = group
				})
				updated++
				ids = append(ids, exist.ID)
				continue
			}
			skipped = append(skipped, skippedItem{URL: norm, Reason: "已存在"})
			continue
		}
		nm := name
		if nm == "" {
			nm = parser.HostOf(norm)
		}
		src := model.Source{
			Name: nm, URL: norm, Group: group,
			Enabled: enabled, Type: model.TypeUnknown, Status: model.StatusUnknown,
		}
		if err := a.st.Add(&src); err != nil {
			skipped = append(skipped, skippedItem{URL: norm, Reason: err.Error()})
			continue
		}
		addedCount++
		added = append(added, src)
		ids = append(ids, src.ID)
	}
	if skipped == nil {
		skipped = []skippedItem{}
	}
	if ids == nil {
		ids = []string{}
	}

	// 批量/文件导入以前完全不探活：源会长时间停在「未知类型」，多仓地址被当成单仓下发。
	// 这里补上（默认开，可用 autoCheck:false 关掉），类型定型后站点源立刻生成。
	autoCheck := true
	if req.AutoCheck != nil {
		autoCheck = *req.AutoCheck
	}
	if autoCheck && len(added) > 0 {
		go a.asyncCheckSources(added)
	}

	ok(w, map[string]interface{}{
		"added": addedCount, "updated": updated, "skipped": skipped, "ids": ids,
	})
}

// asyncCheckSources 批量探活（并发受限 + 攒批落盘）。
// 结果回写后 Store 会自动按类型生成/替换站点源。
func (a *API) asyncCheckSources(srcs []model.Source) {
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

	for _, s := range srcs {
		s := s
		a.st.UpdateInMemory(s.ID, func(sp *model.Source) { sp.Status = model.StatusChecking })
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
				log.Printf("[api] 批量探活 panic（已隔离）: %v", rec)
				// 状态已被置成「检测中」，不给个结果就会永久卡在那个状态
				func() {
					defer func() { _ = recover() }()
					_ = a.st.UpdateCheckResult(s.ID, store.CheckResult{
						Status: model.StatusFail, Msg: "探活异常中断",
					}, false)
				}()
			}()
			res, _ := ck.FetchT(context.Background(), s.URL, false, s.TimeoutSeconds)
			if res == nil {
				res = &checker.Result{Status: model.StatusFail, Msg: "探活无结果"}
			}
			_ = a.st.UpdateCheckResult(s.ID, store.CheckResult{
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
	log.Printf("[api] 批量探活完成: %d 条", len(srcs))
}

// splitNameURL 兼容「名称,URL」「名称|URL」「纯 URL」三种行格式。
// 以 http 开头的一律视为纯 URL；否则按首个 , 或 | 拆分。
func splitNameURL(line string) (string, string) {
	s := strings.TrimSpace(line)
	if s == "" || strings.HasPrefix(s, "#") {
		return "", ""
	}
	if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") {
		return "", s
	}
	for _, sep := range []string{"|", ","} {
		if i := strings.Index(s, sep); i > 0 {
			return strings.TrimSpace(s[:i]), strings.TrimSpace(s[i+1:])
		}
	}
	return "", s
}

// handleSourceImport 文件导入：multipart/form-data 字段 file，.json / .txt
func (a *API) handleSourceImport(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<20) // multipart 总量上限
	if err := r.ParseMultipartForm(2 << 20); err != nil {
		badRequest(w, "文件上传失败（上限 2MB）: "+err.Error())
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		badRequest(w, "缺少 file 字段")
		return
	}
	defer file.Close()
	if header.Size > 2<<20 {
		badRequest(w, "文件超过 2MB 上限")
		return
	}
	body, err := io.ReadAll(io.LimitReader(file, 2<<20+1))
	if err != nil {
		badRequest(w, "读取文件失败: "+err.Error())
		return
	}
	group := strings.TrimSpace(r.FormValue("group"))
	if group == "" {
		group = "默认"
	}
	enabled := r.FormValue("enabled") != "false"

	text := string(body)
	lower := strings.ToLower(header.Filename)

	var items []batchItem
	if strings.HasSuffix(lower, ".txt") || !jsonLooksLike(text) {
		for _, e := range strings.Split(text, "\n") {
			if strings.TrimSpace(e) == "" {
				continue
			}
			n, u := splitNameURL(e)
			items = append(items, batchItem{Name: n, URL: u})
		}
	} else {
		parsed, perr := ParseSourceJSON(text)
		if perr != nil {
			badRequest(w, perr.Error())
			return
		}
		for _, e := range parsed.Entries {
			items = append(items, batchItem{Name: e.Name, URL: e.URL})
		}
	}

	// 复用批量添加逻辑
	enabledPtr := enabled
	a.batchAdd(w, batchAddReq{Items: items, Group: group, Enabled: &enabledPtr, OnDuplicate: "skip"})
}

func jsonLooksLike(s string) bool {
	t := strings.TrimLeft(s, " \t\r\n\ufeff")
	return strings.HasPrefix(t, "{") || strings.HasPrefix(t, "[")
}

func (a *API) handleSourceGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	src, found := a.st.Get(id)
	if !found {
		notFound(w, "源不存在")
		return
	}
	ok(w, src)
}

type updateSourceReq struct {
	Name           *string `json:"name"`
	URL            *string `json:"url"`
	Group          *string `json:"group"`
	Remark         *string `json:"remark"`
	Enabled        *bool   `json:"enabled"`
	Sort           *int    `json:"sort"`
	TimeoutSeconds *int    `json:"timeoutSeconds"` // 本条单独的探活超时，0=用全局
}

func (a *API) handleSourceUpdate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cur, found := a.st.Get(id)
	if !found {
		notFound(w, "源不存在")
		return
	}
	var req updateSourceReq
	if !decodeJSON(w, r, &req) {
		return
	}
	// 改地址前先查重：放行的话会出现两条同 URL 的源，站点源会被合成一条、
	// 删掉其中一条又会把另一条的站点源一起摘掉
	if req.URL != nil {
		if norm, err := parser.Normalize(*req.URL); err == nil && norm != cur.URL {
			if other, exists := a.st.GetByURL(norm); exists && other.ID != id {
				writeErr(w, http.StatusConflict, 409, "该地址已存在（与另一个源重复）")
				return
			}
		}
	}
	err := a.st.Update(id, func(sp *model.Source) {
		if req.Name != nil {
			sp.Name = strings.TrimSpace(*req.Name)
		}
		if req.URL != nil {
			norm, err := parser.Normalize(*req.URL)
			if err == nil && norm != sp.URL {
				sp.URL = norm
				// 换了地址，旧的解析结果就作废了：不清掉的话 Type 还是 multi、
				// Detail 还是老仓库的子仓列表，站点源会被继续按旧子仓地址生成，
				// 新地址反而进不了订阅（要等下一轮探活才修正）。
				sp.Type = model.TypeUnknown
				sp.Detail = nil
				sp.Status = model.StatusUnknown
				sp.CheckMsg = ""
				sp.HTTPCode = 0
				sp.LatencyMs = 0
				// 换地址等于换了个源，旧的连续失败/成功次数不能带过来，
				// 否则新地址第一次失败就可能撞上自动禁用阈值
				sp.FailStreak = 0
				sp.OKStreak = 0
			}
		}
		if req.Group != nil {
			sp.Group = strings.TrimSpace(*req.Group)
			if sp.Group == "" {
				sp.Group = "默认"
			}
		}
		if req.Remark != nil {
			sp.Remark = strings.TrimSpace(*req.Remark)
		}
		if req.Enabled != nil {
			sp.Enabled = *req.Enabled
		}
		if req.Sort != nil {
			sp.Sort = *req.Sort
		}
		if req.TimeoutSeconds != nil {
			sp.TimeoutSeconds = *req.TimeoutSeconds
		}
	})
	if err != nil {
		serverError(w, err)
		return
	}
	updated, _ := a.st.Get(id)
	ok(w, updated)
}

func (a *API) handleSourceDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if n := a.st.Delete([]string{id}); n == 0 {
		notFound(w, "源不存在")
		return
	}
	ok(w, map[string]interface{}{"deleted": id})
}

func (a *API) handleSourceToggle(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	err := a.st.Update(id, func(sp *model.Source) { sp.Enabled = !sp.Enabled })
	if err != nil {
		notFound(w, "源不存在")
		return
	}
	src, _ := a.st.Get(id)
	ok(w, src)
}

type batchActionReq struct {
	IDs    []string `json:"ids"`
	Action string   `json:"action"` // enable / disable / delete / group
	Group  string   `json:"group"`
}

func (a *API) handleBatchAction(w http.ResponseWriter, r *http.Request) {
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
	n, err := a.st.BatchAction(req.IDs, req.Action, strings.TrimSpace(req.Group))
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	ok(w, map[string]interface{}{"affected": n})
}

func (a *API) handleGroups(w http.ResponseWriter, r *http.Request) {
	ok(w, a.st.Groups())
}
