package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"tvbox-manager/internal/model"
	"tvbox-manager/internal/parser"
)

/*
「粘贴 JSON 导入」：把用户复制来的一段 JSON 直接解析成源地址列表。

之所以要有单独的解析器，是因为实际流传的 JSON 形态太杂：
  - TVBox 分享格式   {"urls":[{"url":"…","name":"…"}]}
  - 纯字符串数组     ["http://…","https://…"]
  - 对象数组         [{"name":"…","url":"…"}]
  - 本工具的备份     {"sources":[{…,"url":"…"}]}
  - 单个对象         {"name":"…","url":"…"}
  - 各种包装层       {"data":{…}} / {"list":[…]} / {"result":{…}}
最后再兜一层「全量深扫」：只要对象里有 url 字符串字段就当成一条源。

必须避开的坑：TVBox 仓库本身（{"sites":[…],"lives":[…]}）里也有大量 url，
但那是站点接口和直播频道，不是「源地址」。硬扫会把几百条直播地址灌进来，
所以这些键统一进黑名单，且一旦识别成仓库内容就明确报错引导用户。
*/

// jsonEntry 一条待导入的源地址
type jsonEntry struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// parsedJSON 解析结果
type parsedJSON struct {
	Kind    string      `json:"kind"` // urls / array / sources / single / scan
	Entries []jsonEntry `json:"entries"`
}

// 仓库内容里的非源地址键：命中即跳过（不参与深扫）
var jsonSkipKeys = map[string]bool{
	"sites": true, "lives": true, "parses": true, "flags": true,
	"wallpaper": true, "spider": true, "ads": true, "ijk": true,
	"doh": true, "rules": true, "hosts": true, "headers": true,
	"ext": true, "danmaku": true, "proxy": true, "logo": true,
	"notice": true, "sitemap": true, "icons": true, "live": true,
}

// 常见的包装键：优先按这些键下钻，避免把同级无关字段也扫进来
var jsonWrapKeys = []string{"urls", "sources", "data", "list", "items", "result", "rows", "content"}

// ParseSourceJSON 解析任意常见 JSON 形态为源地址列表。
// 返回值 kind 用于前端做差异化提示（例如 sources 说明是备份数据）。
func ParseSourceJSON(text string) (*parsedJSON, error) {
	trimmed := strings.TrimSpace(strings.TrimPrefix(text, "\ufeff"))
	if trimmed == "" {
		return nil, fmt.Errorf("内容为空")
	}
	if len(trimmed) > 4<<20 {
		return nil, fmt.Errorf("内容超过 4MB 上限")
	}
	// TVBox 社区流传的配置普遍是「带注释的 JSONC」：// 与 /* */ 注释、末尾悬挂逗号。
	// 先清洗成标准 JSON 再解析；无注释时清洗是纯透传，对标准 JSON 零影响。
	cleaned := sanitizeJSONText(trimmed)
	if !json.Valid([]byte(cleaned)) {
		return nil, fmt.Errorf("JSON 格式不合法：可能复制不完整，或缺少右括号")
	}

	var root interface{}
	if err := json.Unmarshal([]byte(cleaned), &root); err != nil {
		return nil, fmt.Errorf("JSON 解析失败: %v", err)
	}

	sc := &jsonScanner{seen: map[string]bool{}}
	kind := ""

	switch v := root.(type) {
	case []interface{}:
		kind = "array"
		sc.walk(v, 0)
	case map[string]interface{}:
		if _, hasURLs := v["urls"]; hasURLs {
			kind = "urls"
			sc.walk(v["urls"], 0)
		} else if _, hasSources := v["sources"]; hasSources {
			kind = "sources"
			sc.walk(v["sources"], 0)
		} else if u, isStr := v["url"].(string); isStr && strings.TrimSpace(u) != "" {
			kind = "single"
			sc.walk(v, 0)
		} else {
			kind = "scan"
			sc.walkObject(v, 0)
		}
	}

	if len(sc.entries) == 0 {
		if sc.repoLike {
			return nil, fmt.Errorf("这段 JSON 是 TVBox 仓库内容（含 sites / lives），不是地址列表；请把它托管成 URL 后添加为源")
		}
		if kind == "single" || kind == "scan" || kind == "urls" || kind == "array" || kind == "sources" {
			return nil, fmt.Errorf("未识别到任何 url 条目（需要形如 {\"url\":\"http://…\"} 或 [\"http://…\"]）")
		}
		return nil, fmt.Errorf("无法识别的 JSON 结构")
	}
	return &parsedJSON{Kind: kind, Entries: sc.entries}, nil
}

type jsonScanner struct {
	entries  []jsonEntry
	seen     map[string]bool
	repoLike bool
	depth    int
}

func (sc *jsonScanner) add(name, rawURL string) {
	u := strings.TrimSpace(rawURL)
	if u == "" {
		return
	}
	key := strings.ToLower(u)
	if sc.seen[key] {
		return
	}
	sc.seen[key] = true
	sc.entries = append(sc.entries, jsonEntry{Name: strings.TrimSpace(name), URL: u})
}

// walk 遍历任意 JSON 值
func (sc *jsonScanner) walk(v interface{}, depth int) {
	if depth > 8 {
		return
	}
	switch t := v.(type) {
	case []interface{}:
		for _, x := range t {
			sc.walk(x, depth+1)
		}
	case string:
		if looksLikeURL(t) {
			sc.add("", t)
		}
	case map[string]interface{}:
		sc.walkObject(t, depth+1)
	}
}

func (sc *jsonScanner) walkObject(obj map[string]interface{}, depth int) {
	if depth > 8 {
		return
	}
	// 1. 自身就是一条源
	if u, ok := obj["url"].(string); ok && strings.TrimSpace(u) != "" {
		name := ""
		if n, ok := obj["name"].(string); ok {
			name = n
		}
		if name == "" {
			// 备份格式里名称字段可能叫 title / remark
			for _, k := range []string{"title", "remark", "label"} {
				if n, ok := obj[k].(string); ok && strings.TrimSpace(n) != "" {
					name = n
					break
				}
			}
		}
		sc.add(name, u)
		return
	}
	// 2. 仓库内容特征：记录一下，便于给出更准确的报错
	if _, ok := obj["sites"]; ok {
		sc.repoLike = true
	}
	// 3. 常见包装键优先
	for _, k := range jsonWrapKeys {
		if child, ok := obj[k]; ok && !jsonSkipKeys[k] {
			sc.walk(child, depth+1)
			return
		}
	}
	// 4. 兜底：扫所有非黑名单字段
	for k, child := range obj {
		if jsonSkipKeys[strings.ToLower(k)] {
			continue
		}
		sc.walk(child, depth+1)
	}
}

func looksLikeURL(s string) bool {
	t := strings.TrimSpace(s)
	return strings.HasPrefix(t, "http://") || strings.HasPrefix(t, "https://")
}

// sanitizeJSONText 把 JSONC 清洗成标准 JSON：去掉注释 + 悬挂逗号。
// 逐字符状态机扫描：字符串内部的 // 和 /* 原样保留（URL 里全是 /，必须严格区分内外）。
// 无注释、无悬挂逗号时输出与输入逐字节相同，对标准 JSON 零影响。
// 注意：悬挂逗号与闭合符之间可能隔着换行/空格（",\n  ]"），不能只查 ",}" 字面量；
// 且注释剥掉后可能暴露出新悬挂逗号（"1, /* c */\n}" → "1, \n}"），所以剥完要复查。
func sanitizeJSONText(s string) string {
	hasComment := strings.Contains(s, "//") || strings.Contains(s, "/*")
	if !hasComment && !hasTrailingComma(s) {
		return s // 快速通道：纯透传
	}
	if hasComment {
		s = stripJSONComments(s)
	}
	if hasTrailingComma(s) {
		s = removeTrailingCommas(s)
	}
	return s
}

// hasTrailingComma 只探测不输出：是否存在悬挂逗号（零分配）。
func hasTrailingComma(s string) bool {
	inStr, esc := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case ',':
			j := i + 1
			for j < len(s) && (s[j] == ' ' || s[j] == '\t' || s[j] == '\r' || s[j] == '\n') {
				j++
			}
			if j < len(s) && (s[j] == '}' || s[j] == ']') {
				return true
			}
		}
	}
	return false
}

// stripJSONComments 去掉 // 行注释与 /* */ 块注释；字符串字面量内部不动。
func stripJSONComments(s string) string {
	if !strings.Contains(s, "//") && !strings.Contains(s, "/*") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	inStr, esc := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			b.WriteByte(c)
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch {
		case c == '"':
			inStr = true
			b.WriteByte(c)
		case c == '/' && i+1 < len(s) && s[i+1] == '/': // 行注释：吃掉到行尾，保留换行
			for i++; i < len(s) && s[i] != '\n'; i++ {
			}
			if i < len(s) {
				b.WriteByte('\n')
			}
		case c == '/' && i+1 < len(s) && s[i+1] == '*': // 块注释：吃掉到 */，用一个空格占位防粘连
			i += 2
			for ; i+1 < len(s) && !(s[i] == '*' && s[i+1] == '/'); i++ {
			}
			if i+1 < len(s) {
				i++ // 指向 '/'，外层 i++ 越过它
			}
			b.WriteByte(' ')
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// removeTrailingCommas 去掉 } / ] 前的悬挂逗号（JSONC 常见写法，允许中间隔空白）。
// 无悬挂逗号时原样返回（零拷贝）。
func removeTrailingCommas(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	changed := false
	inStr, esc := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			b.WriteByte(c)
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		if c == '"' {
			inStr = true
			b.WriteByte(c)
			continue
		}
		if c == ',' { // 逗号后只隔空白就是闭合符 → 悬挂逗号，跳过
			j := i + 1
			for j < len(s) && (s[j] == ' ' || s[j] == '\t' || s[j] == '\r' || s[j] == '\n') {
				j++
			}
			if j < len(s) && (s[j] == '}' || s[j] == ']') {
				changed = true
				continue
			}
		}
		b.WriteByte(c)
	}
	if !changed {
		return s
	}
	return b.String()
}

// ---- HTTP 接口 ----

type jsonImportReq struct {
	Text    string `json:"text"`
	Name    string `json:"name"` // 单仓托管时的源名称，留空自动取名
	Group   string `json:"group"`
	Enabled *bool  `json:"enabled"`
	DryRun  bool   `json:"dryRun"` // true：只解析预览，不落库
}

type jsonPreviewRow struct {
	Name   string `json:"name"`
	URL    string `json:"url"`
	Dup    bool   `json:"dup"`
	Reason string `json:"reason,omitempty"`
}

// handleSourceImportJSON POST /api/sources/import/json
// 粘贴 JSON 导入：dryRun 出预览（识别类型 + 条数 + 重复数），否则落库。
func (a *API) handleSourceImportJSON(w http.ResponseWriter, r *http.Request) {
	var req jsonImportReq
	if !decodeJSON(w, r, &req) {
		return
	}
	a.importContent(w, r, req.Text, req.Name, req.Group, req.Enabled, req.DryRun)
}

// importContent 粘贴/文件导入共用的智能识别入口，三步分流：
//  1. 先按「仓库内容」识别（前后带杂文时 CleanJSON 会提取嵌入的 JSON 段）：
//     单仓 → 本服务托管为源；多仓 → 只提取子仓地址，其余丢弃；
//  2. 不是仓库结构 → 按地址列表解析（ParseSourceJSON，TVBox 分享格式 / 数组 / 备份 / 深扫）；
//  3. 都不行 → 明确报错。
func (a *API) importContent(w http.ResponseWriter, r *http.Request, text, name, group string, enabled *bool, dryRun bool) {
	text = strings.TrimSpace(strings.TrimPrefix(text, "\ufeff"))
	if text == "" {
		badRequest(w, "内容为空")
		return
	}
	if len(text) > 4<<20 {
		badRequest(w, "内容超过 4MB 上限")
		return
	}

	cleaned := string(parser.CleanJSON([]byte(text)))

	typ, detail, err := parser.Parse([]byte(cleaned))
	if err == nil {
		switch typ {
		case model.TypeSingle:
			a.importRepoSingle(w, r, cleaned, detail, name, group, enabled, dryRun)
			return
		case model.TypeMulti:
			a.importRepoMulti(w, detail, group, enabled, dryRun)
			return
		}
	}

	parsed, perr := ParseSourceJSON(cleaned)
	if perr != nil {
		badRequest(w, perr.Error())
		return
	}
	a.importURLList(w, parsed, group, enabled, dryRun)
}

// previewRows 地址条目 → 预览行（归一化 + 重复标记）
func (a *API) previewRows(entries []jsonEntry) ([]jsonPreviewRow, int) {
	rows := make([]jsonPreviewRow, 0, len(entries))
	dup := 0
	for _, e := range entries {
		norm, nerr := parser.Normalize(e.URL)
		if nerr != nil {
			rows = append(rows, jsonPreviewRow{Name: e.Name, URL: e.URL, Reason: "地址非法: " + nerr.Error()})
			continue
		}
		_, found := a.st.GetByURL(norm)
		if found {
			dup++
		}
		rows = append(rows, jsonPreviewRow{Name: e.Name, URL: norm, Dup: found})
	}
	return rows, dup
}

// importURLList 地址列表类：预览或批量添加
func (a *API) importURLList(w http.ResponseWriter, parsed *parsedJSON, group string, enabled *bool, dryRun bool) {
	if dryRun {
		rows, dup := a.previewRows(parsed.Entries)
		ok(w, map[string]interface{}{
			"kind": parsed.Kind, "total": len(rows), "dup": dup, "entries": rows,
		})
		return
	}
	items := make([]batchItem, 0, len(parsed.Entries))
	for _, e := range parsed.Entries {
		items = append(items, batchItem{Name: e.Name, URL: e.URL})
	}
	a.batchAdd(w, batchAddReq{Items: items, Group: group, Enabled: enabled, OnDuplicate: "skip"})
}

// importRepoMulti 多仓配置：只提取子仓地址（名称+URL）导入，其余结构（spider/直播/壁纸等）丢弃
func (a *API) importRepoMulti(w http.ResponseWriter, detail *model.Detail, group string, enabled *bool, dryRun bool) {
	seen := map[string]bool{}
	entries := make([]jsonEntry, 0, len(detail.SubURLs))
	for _, s := range detail.SubURLs {
		key := strings.ToLower(strings.TrimSpace(s.URL))
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		entries = append(entries, jsonEntry{Name: s.Name, URL: s.URL})
	}
	if len(entries) == 0 {
		badRequest(w, "多仓配置中未提取到子仓地址")
		return
	}
	if dryRun {
		rows, dup := a.previewRows(entries)
		ok(w, map[string]interface{}{
			"kind": "repo-multi", "total": len(rows), "dup": dup, "entries": rows,
			"note": "识别为多仓配置：仅提取其中的子仓地址导入，其余内容（spider/直播/壁纸等）忽略",
		})
		return
	}
	items := make([]batchItem, 0, len(entries))
	for _, e := range entries {
		items = append(items, batchItem{Name: e.Name, URL: e.URL})
	}
	a.batchAdd(w, batchAddReq{Items: items, Group: group, Enabled: enabled, OnDuplicate: "skip"})
}

// importRepoSingle 单仓配置：本服务托管为源（保存内容 → /configs/{id}.json → 建源 → 自动探活）
func (a *API) importRepoSingle(w http.ResponseWriter, r *http.Request, cleaned string, detail *model.Detail, name, group string, enabled *bool, dryRun bool) {
	summary := fmt.Sprintf("%d 站点 / %d 直播 / %d 解析", detail.Sites, detail.Lives, detail.Parses)
	if dryRun {
		siteNames := detail.SiteNames
		if len(siteNames) > 10 {
			siteNames = siteNames[:10]
		}
		ok(w, map[string]interface{}{
			"kind": "repo-single", "total": 1, "dup": 0, "entries": []jsonPreviewRow{},
			"summary": summary, "siteNames": siteNames, "selfHost": true,
			"note": "识别为单仓配置：导入后由本服务托管为本地源并自动探活，站点源照常生成进订阅；删除该源即删除托管文件",
		})
		return
	}
	if a.PastedDir == "" {
		badRequest(w, "服务端未启用本地托管，无法导入单仓配置")
		return
	}
	fname, err := a.savePastedConfig(cleaned)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, envelope{Code: 500, Msg: "托管文件保存失败: " + err.Error()})
		return
	}
	url := baseURL(r) + "/configs/" + fname

	nm := strings.TrimSpace(name)
	if nm == "" {
		nm = "粘贴单仓"
	}
	gr := strings.TrimSpace(group)
	if gr == "" {
		gr = "默认"
	}
	en := true
	if enabled != nil {
		en = *enabled
	}
	src := model.Source{
		Name: nm, URL: url, Group: gr, Enabled: en,
		Type: model.TypeUnknown, Status: model.StatusUnknown,
		Remark: "粘贴内容本地托管",
	}
	if err := a.st.Add(&src); err != nil {
		a.cleanupPasted(url) // 建源失败不留孤儿文件
		badRequest(w, err.Error())
		return
	}
	go a.asyncCheckOne(src.ID, src.URL, src.TimeoutSeconds)
	ok(w, map[string]interface{}{
		"kind": "repo-single", "added": 1, "id": src.ID,
		"selfHosted": url, "summary": summary,
	})
}
