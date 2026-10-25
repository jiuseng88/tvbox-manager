package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

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
	}
	fmt.Sscanf(q.Get("page"), "%d", &f.Page)
	fmt.Sscanf(q.Get("size"), "%d", &f.Size)
	if f.Size <= 0 {
		f.Size = 50
	}
	if f.Size > 500 {
		f.Size = 500
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
	Name      string `json:"name"`
	URL       string `json:"url"`
	Group     string `json:"group"`
	Remark    string `json:"remark"`
	Enabled   *bool  `json:"enabled"`
	AutoCheck *bool  `json:"autoCheck"`
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
		Name:  strings.TrimSpace(req.Name),
		URL:   norm,
		Group: strings.TrimSpace(req.Group),
		Remark: strings.TrimSpace(req.Remark),
		Type:  model.TypeUnknown,
		Status: model.StatusUnknown,
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
		a.asyncCheckOne(src.ID, src.URL)
	}
	created, _ := a.st.Get(src.ID)
	ok(w, created)
}

type batchAddReq struct {
	URLs        []string `json:"urls"`
	Group       string   `json:"group"`
	Enabled     *bool    `json:"enabled"`
	OnDuplicate string   `json:"onDuplicate"` // skip(默认) / update
}

func (a *API) handleSourceBatch(w http.ResponseWriter, r *http.Request) {
	var req batchAddReq
	if !decodeJSON(w, r, &req) {
		return
	}
	a.batchAdd(w, req)
}

func (a *API) batchAdd(w http.ResponseWriter, req batchAddReq) {
	if len(req.URLs) == 0 {
		badRequest(w, "urls 不能为空")
		return
	}
	if len(req.URLs) > 2000 {
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
	added := 0
	updated := 0

	for _, raw := range req.URLs {
		name, url := splitNameURL(raw)
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
		added++
		ids = append(ids, src.ID)
	}
	if skipped == nil {
		skipped = []skippedItem{}
	}
	if ids == nil {
		ids = []string{}
	}
	ok(w, map[string]interface{}{
		"added": added, "updated": updated, "skipped": skipped, "ids": ids,
	})
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

	var entries []string
	if strings.HasSuffix(lower, ".txt") || !jsonLooksLike(text) {
		entries = strings.Split(text, "\n")
	} else {
		entries, err = importFromJSON(text)
		if err != nil {
			badRequest(w, err.Error())
			return
		}
	}

	// 复用批量添加逻辑
	urls := make([]string, 0, len(entries))
	for _, e := range entries {
		if strings.TrimSpace(e) == "" {
			continue
		}
		urls = append(urls, e)
	}
	enabledPtr := enabled
	a.batchAdd(w, batchAddReq{URLs: urls, Group: group, Enabled: &enabledPtr, OnDuplicate: "skip"})
}

func jsonLooksLike(s string) bool {
	t := strings.TrimLeft(s, " \t\r\n\ufeff")
	return strings.HasPrefix(t, "{") || strings.HasPrefix(t, "[")
}
// importFromJSON 解析三种 JSON 形态为行数组
func importFromJSON(text string) ([]string, error) {
	trimmed := strings.TrimSpace(text)
	if strings.HasPrefix(trimmed, "[") {
		var arr []struct {
			URL  string `json:"url"`
			Name string `json:"name"`
		}
		if err := json.Unmarshal([]byte(trimmed), &arr); err != nil {
			return nil, fmt.Errorf("JSON 数组解析失败: %v", err)
		}
		out := make([]string, 0, len(arr))
		for _, x := range arr {
			if x.URL == "" {
				continue
			}
			if x.Name != "" {
				out = append(out, x.Name+","+x.URL)
			} else {
				out = append(out, x.URL)
			}
		}
		return out, nil
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal([]byte(trimmed), &probe); err != nil {
		return nil, fmt.Errorf("JSON 解析失败: %v", err)
	}
	if _, hasURLs := probe["urls"]; hasURLs {
		var o struct {
			URLs []struct {
				URL  string `json:"url"`
				Name string `json:"name"`
			} `json:"urls"`
		}
		if err := json.Unmarshal([]byte(trimmed), &o); err != nil {
			return nil, fmt.Errorf("urls 解析失败: %v", err)
		}
		out := make([]string, 0, len(o.URLs))
		for _, x := range o.URLs {
			if x.URL == "" {
				continue
			}
			if x.Name != "" {
				out = append(out, x.Name+","+x.URL)
			} else {
				out = append(out, x.URL)
			}
		}
		return out, nil
	}
	if _, hasSites := probe["sites"]; hasSites {
		return nil, fmt.Errorf("该文件是仓库内容而非地址列表，请改用「粘贴 JSON 内容」方式或直接把该文件托管地址添加为源")
	}
	return nil, fmt.Errorf("无法识别的 JSON 结构（缺少 urls）")
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
	Name   *string `json:"name"`
	URL    *string `json:"url"`
	Group  *string `json:"group"`
	Remark *string `json:"remark"`
	Enabled *bool  `json:"enabled"`
	Sort   *int    `json:"sort"`
}

func (a *API) handleSourceUpdate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, found := a.st.Get(id); !found {
		notFound(w, "源不存在")
		return
	}
	var req updateSourceReq
	if !decodeJSON(w, r, &req) {
		return
	}
	err := a.st.Update(id, func(sp *model.Source) {
		if req.Name != nil {
			sp.Name = strings.TrimSpace(*req.Name)
		}
		if req.URL != nil {
			norm, err := parser.Normalize(*req.URL)
			if err == nil {
				sp.URL = norm
				sp.Status = model.StatusUnknown
				sp.CheckMsg = ""
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
