package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

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
// 粘贴 JSON 导入：dryRun 出预览（识别条数 + 重复条数），否则直接灌进批量添加。
func (a *API) handleSourceImportJSON(w http.ResponseWriter, r *http.Request) {
	var req jsonImportReq
	if !decodeJSON(w, r, &req) {
		return
	}
	parsed, err := ParseSourceJSON(req.Text)
	if err != nil {
		badRequest(w, err.Error())
		return
	}

	if req.DryRun {
		rows := make([]jsonPreviewRow, 0, len(parsed.Entries))
		dup := 0
		for _, e := range parsed.Entries {
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
		ok(w, map[string]interface{}{
			"kind": parsed.Kind, "total": len(rows), "dup": dup, "entries": rows,
		})
		return
	}

	items := make([]batchItem, 0, len(parsed.Entries))
	for _, e := range parsed.Entries {
		items = append(items, batchItem{Name: e.Name, URL: e.URL})
	}
	a.batchAdd(w, batchAddReq{Items: items, Group: req.Group, Enabled: req.Enabled, OnDuplicate: "skip"})
}
