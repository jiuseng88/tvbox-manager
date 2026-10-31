package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"tvbox-manager/internal/model"
	"tvbox-manager/internal/parser"
	"tvbox-manager/internal/store"
)

/*
「源自动刮削」（PRD A-06：TVBox 源自动刮削，L1+L2 手动闭环）：

  - 手动收录（L1）：填一个订阅地址 → 服务端拉取（复用 checker.Fetch，SSRF 强制开启）
    → 走与「粘贴/文件导入」同一套智能识别（单仓托管 / 多仓提子仓 / 地址列表 / txt 行列表）
    → dryRun 预览去重，或直接批量导入。
  - 种子同步（L2）：维护种子清单（源集合站点 / 公开仓库直链），一次同步把每个种子的
    候选条目写入候选区；候选经用户确认后才导入为源（先候选后确认，不自动入库）。
  - 合规约束：来源仅限用户主动添加的种子 URL；全链路 SSRF 防护；同步不自动删除
    已有源；已存在（dup）的候选标记出来但默认不重复入库。

    IPTV 源（A-07）后续在 internal/live 解析层接入同一套「同步 → 候选 → 确认」闭环。
*/

// scrapeSyncMu 同一时刻只允许一个同步任务（避免多个种子并发打上游、候选区互相覆盖）
var scrapeSyncMu sync.Mutex

// ---- 手动收录（L1） ----

type scrapeImportReq struct {
	URL     string `json:"url"`
	Name    string `json:"name"` // 单仓托管/导入时的源名称，留空自动取名
	Group   string `json:"group"`
	Enabled *bool  `json:"enabled"`
	DryRun  bool   `json:"dryRun"` // true：只拉取解析预览，不落库
}

// handleScrapeImport POST /api/scrape/import
// 填订阅地址 → 服务端拉取 → 智能识别 → 预览（dryRun）或导入。
func (a *API) handleScrapeImport(w http.ResponseWriter, r *http.Request) {
	var req scrapeImportReq
	if !decodeJSON(w, r, &req) {
		return
	}
	raw := strings.TrimSpace(req.URL)
	if raw == "" {
		badRequest(w, "url 不能为空")
		return
	}
	res, body := a.sch.Checker().Fetch(r.Context(), raw, true) // guardPrivate=true：SSRF 强制
	if res.Status != model.StatusOK {
		ok(w, map[string]interface{}{
			"ok": false, "errMsg": res.Msg, "httpCode": res.HTTPCode, "latencyMs": res.LatencyMs,
		})
		return
	}
	text := string(body)
	if jsonLooksLike(text) {
		// JSON 内容：与粘贴/文件导入完全同一套智能识别（单仓托管/多仓提子仓/地址列表）
		a.importContent(w, r, text, req.Name, req.Group, req.Enabled, req.DryRun)
		return
	}
	// 非 JSON：按 txt 行列表处理（兼容「名称,URL」「名称|URL」「纯 URL」）
	entries := make([]jsonEntry, 0, 16)
	seen := map[string]bool{}
	for _, line := range strings.Split(text, "\n") {
		n, u := splitNameURL(line)
		if u == "" || !looksLikeURL(u) || seen[strings.ToLower(u)] {
			continue
		}
		seen[strings.ToLower(u)] = true
		entries = append(entries, jsonEntry{Name: n, URL: u})
	}
	if len(entries) == 0 {
		badRequest(w, "拉取到的内容既不是 JSON，也识别不到任何地址行")
		return
	}
	if req.DryRun {
		rows, dup := a.previewRows(entries)
		ok(w, map[string]interface{}{"kind": "txt", "total": len(rows), "dup": dup, "entries": rows})
		return
	}
	items := make([]batchItem, 0, len(entries))
	for _, e := range entries {
		items = append(items, batchItem{Name: e.Name, URL: e.URL})
	}
	a.batchAdd(w, batchAddReq{Items: items, Group: req.Group, Enabled: req.Enabled, OnDuplicate: "skip"})
}

// ---- 种子清单 CRUD ----

type scrapeSeedReq struct {
	Name    string `json:"name"`
	URL     string `json:"url"`
	Type    string `json:"type"` // tvbox（源地址刮削）/ live（直播播放列表刮削），空 = tvbox
	Group   string `json:"group"`
	Enabled *bool  `json:"enabled"`
	Remark  string `json:"remark"`
}

func (a *API) handleScrapeSeedList(w http.ResponseWriter, r *http.Request) {
	ok(w, a.st.ListScrapeSeeds())
}

func (a *API) handleScrapeSeedAdd(w http.ResponseWriter, r *http.Request) {
	var req scrapeSeedReq
	if !decodeJSON(w, r, &req) {
		return
	}
	norm, err := parser.Normalize(req.URL)
	if err != nil {
		badRequest(w, "地址非法: "+err.Error())
		return
	}
	for _, s := range a.st.ListScrapeSeeds() {
		if s.URL == norm {
			writeErr(w, http.StatusConflict, 409, "该种子地址已存在")
			return
		}
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = parser.HostOf(norm)
	}
	group := strings.TrimSpace(req.Group)
	if group == "" {
		group = "默认"
	}
	stype := strings.TrimSpace(req.Type)
	if stype == "" {
		stype = "tvbox"
	}
	if stype != "tvbox" && stype != "live" {
		badRequest(w, "type 仅支持 tvbox / live")
		return
	}
	en := true
	if req.Enabled != nil {
		en = *req.Enabled
	}
	created, err := a.st.AddScrapeSeed(&model.ScrapeSeed{
		Name: name, URL: norm, Type: stype, Group: group, Enabled: en, Remark: strings.TrimSpace(req.Remark),
	})
	if err != nil {
		serverError(w, err)
		return
	}
	ok(w, created)
}

type scrapeSeedUpdateReq struct {
	Name    *string `json:"name"`
	URL     *string `json:"url"`
	Type    *string `json:"type"` // tvbox / live，空 = 不改
	Group   *string `json:"group"`
	Enabled *bool   `json:"enabled"`
	Remark  *string `json:"remark"`
}

func (a *API) handleScrapeSeedUpdate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, found := a.st.GetScrapeSeed(id); !found {
		notFound(w, "种子不存在")
		return
	}
	var req scrapeSeedUpdateReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Type != nil {
		st := strings.TrimSpace(*req.Type)
		if st != "tvbox" && st != "live" {
			badRequest(w, "type 仅支持 tvbox / live")
			return
		}
	}
	err := a.st.UpdateScrapeSeed(id, func(sp *model.ScrapeSeed) {
		if req.Name != nil {
			sp.Name = strings.TrimSpace(*req.Name)
		}
		if req.URL != nil {
			if norm, nerr := parser.Normalize(*req.URL); nerr == nil {
				sp.URL = norm
			}
		}
		if req.Type != nil {
			sp.Type = strings.TrimSpace(*req.Type)
		}
		if req.Group != nil {
			sp.Group = strings.TrimSpace(*req.Group)
			if sp.Group == "" {
				sp.Group = "默认"
			}
		}
		if req.Enabled != nil {
			sp.Enabled = *req.Enabled
		}
		if req.Remark != nil {
			sp.Remark = strings.TrimSpace(*req.Remark)
		}
	})
	if err != nil {
		notFound(w, err.Error())
		return
	}
	updated, _ := a.st.GetScrapeSeed(id)
	ok(w, updated)
}

func (a *API) handleScrapeSeedDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !a.st.DeleteScrapeSeed(id) {
		notFound(w, "种子不存在")
		return
	}
	ok(w, map[string]interface{}{"deleted": id})
}

// ---- 种子同步（L2） ----

type scrapeSyncStat struct {
	Seeds      int      `json:"seeds"`      // 启用的种子数
	Synced     int      `json:"synced"`     // 同步成功的种子数
	Candidates int      `json:"candidates"` // 新增候选条数
	Duplicates int      `json:"duplicates"` // 其中已存在（dup）的条数
	Errors     []string `json:"errors,omitempty"`
}

// handleScrapeSync POST /api/scrape/sync
// 遍历启用的种子，逐个拉取 → 识别条目 → 归一化去重 → 写入候选区（不自动入库）。
func (a *API) handleScrapeSync(w http.ResponseWriter, r *http.Request) {
	if !scrapeSyncMu.TryLock() {
		writeJSON(w, http.StatusConflict, envelope{Code: 409, Msg: "已有同步任务在运行"})
		return
	}
	defer scrapeSyncMu.Unlock()

	seeds := a.st.ListScrapeSeeds()
	stat := scrapeSyncStat{}
	now := time.Now()

	for i := range seeds {
		sd := seeds[i]
		if !sd.Enabled {
			continue
		}
		stat.Seeds++
		res, body := a.sch.Checker().Fetch(r.Context(), sd.URL, true) // SSRF 强制
		if res.Status != model.StatusOK {
			msg := "同步失败: " + res.Msg
			stat.Errors = append(stat.Errors, fmt.Sprintf("%s：%s", sd.Name, msg))
			a.st.TouchScrapeSeedSync(sd.ID, msg, now)
			continue
		}
		// 按种子类型分流：tvbox 刮源地址（候选导入源列表）；
		// live 刮直播播放列表（候选导入直播源列表）。
		var entries []jsonEntry
		kind := ""
		target := "source"
		switch sd.Type {
		case "live":
			entries, kind = extractLiveSeedEntries(string(body), sd.Name, sd.URL)
			target = "live"
		default: // "" / tvbox
			entries, kind = extractScrapeEntries(string(body), sd.Name, sd.URL)
		}
		if len(entries) == 0 {
			msg := "同步完成，未识别到任何源地址"
			stat.Errors = append(stat.Errors, fmt.Sprintf("%s：%s", sd.Name, msg))
			a.st.TouchScrapeSeedSync(sd.ID, msg, now)
			continue
		}

		var cands []model.ScrapeCandidate
		seen := map[string]bool{}
		dup := 0
		for _, e := range entries {
			// 直播 URL 可带 | 请求头参数（如 |User-Agent=xx），不做 URL 归一化，按原样判重入库
			storeURL := strings.TrimSpace(e.URL)
			norm, nerr := parser.Normalize(storeURL)
			if nerr == nil {
				storeURL = norm
			}
			if storeURL == "" || seen[strings.ToLower(storeURL)] {
				continue
			}
			seen[strings.ToLower(storeURL)] = true
			exists := false
			if target == "live" {
				exists = a.st.LiveSourceExists(e.URL)
			} else {
				_, exists = a.st.GetByURL(storeURL)
			}
			if exists {
				dup++
			}
			cands = append(cands, model.ScrapeCandidate{
				ID:        store.NewCandidateID(),
				SeedID:    sd.ID,
				SeedName:  sd.Name,
				Name:      e.Name,
				URL:       storeURL,
				Target:    target,
				Kind:      kind,
				Dup:       exists,
				Status:    "pending",
				FetchedAt: now,
			})
		}
		if len(cands) == 0 {
			msg := "同步完成，全部条目已存在或无法归一化"
			a.st.TouchScrapeSeedSync(sd.ID, msg, now)
			continue
		}
		if err := a.st.SetScrapeCandidates(sd.ID, cands); err != nil {
			msg := "候选写入失败: " + err.Error()
			stat.Errors = append(stat.Errors, fmt.Sprintf("%s：%s", sd.Name, msg))
			a.st.TouchScrapeSeedSync(sd.ID, msg, now)
			continue
		}
		stat.Synced++
		stat.Candidates += len(cands)
		stat.Duplicates += dup
		msg := fmt.Sprintf("同步完成：识别 %d 条候选（%d 条已存在）", len(cands), dup)
		a.st.TouchScrapeSeedSync(sd.ID, msg, now)
	}
	// 种子的 LastSyncAt/LastSyncMsg 在内存里更新，统一落盘一次
	if err := a.st.Save(); err != nil {
		log.Printf("[scrape] 同步后落盘失败: %v", err)
	}
	ok(w, stat)
}

// extractScrapeEntries 把拉取到的内容转成候选条目（JSON 智能识别 + txt 行列表兜底）。
// 返回 (条目, kind)，条目为空时调用方按「未识别到内容」处理。
func extractScrapeEntries(text, seedName, seedURL string) ([]jsonEntry, string) {
	text = strings.TrimSpace(strings.TrimPrefix(text, "\ufeff"))
	if text == "" {
		return nil, ""
	}
	cleaned := string(parser.CleanJSON([]byte(text)))

	// 1. 仓库内容：单仓 → 种子本身即一条源；多仓 → 提取子仓地址
	if typ, detail, err := parser.Parse([]byte(cleaned)); err == nil {
		switch typ {
		case model.TypeSingle:
			return []jsonEntry{{Name: seedName, URL: seedURL}}, "repo-single"
		case model.TypeMulti:
			var out []jsonEntry
			seen := map[string]bool{}
			for _, s := range detail.SubURLs {
				u := strings.TrimSpace(s.URL)
				if u == "" || seen[strings.ToLower(u)] {
					continue
				}
				seen[strings.ToLower(u)] = true
				out = append(out, jsonEntry{Name: s.Name, URL: u})
			}
			if len(out) > 0 {
				return out, "repo-multi"
			}
		}
	}
	// 2. 地址列表 JSON（TVBox 分享格式 / 数组 / 备份 / 深扫）
	if parsed, perr := ParseSourceJSON(cleaned); perr == nil && len(parsed.Entries) > 0 {
		return parsed.Entries, parsed.Kind
	}
	// 3. txt 行列表兜底（只接受 http(s) 开头的行，避免把说明文字误当地址）
	var out []jsonEntry
	seen := map[string]bool{}
	for _, line := range strings.Split(text, "\n") {
		n, u := splitNameURL(line)
		if u == "" || !looksLikeURL(u) || seen[strings.ToLower(u)] {
			continue
		}
		seen[strings.ToLower(u)] = true
		out = append(out, jsonEntry{Name: n, URL: u})
	}
	if len(out) > 0 {
		return out, "txt"
	}
	return nil, ""
}

// ---- 候选区 ----

func (a *API) handleScrapeCandidates(w http.ResponseWriter, r *http.Request) {
	ok(w, a.st.ListScrapeCandidates())
}

type scrapeApplyReq struct {
	IDs     []string `json:"ids"`
	Group   string   `json:"group"`
	Enabled *bool    `json:"enabled"`
}

// handleScrapeApply POST /api/scrape/apply
// 把选中的 pending 候选导入：source 候选 → 源列表；live 候选 → 直播源列表。
// 复用批量导入语义（归一化 + 查重 + 自动探活）；导入结果回写候选状态：
// 成功 → applied；已存在/非法 → skipped。
func (a *API) handleScrapeApply(w http.ResponseWriter, r *http.Request) {
	var req scrapeApplyReq
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
	want := make(map[string]bool, len(req.IDs))
	for _, id := range req.IDs {
		want[id] = true
	}

	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	group := strings.TrimSpace(req.Group)
	if group == "" {
		group = "默认"
	}

	type applyItem struct {
		cand model.ScrapeCandidate
	}
	var sourceItems []applyItem
	var liveItems []applyItem
	updates := map[string]string{}
	for _, c := range a.st.ListScrapeCandidates() {
		if !want[c.ID] {
			continue
		}
		if c.Status != "pending" {
			updates[c.ID] = "skipped" // 已处理过：按跳过收尾
			continue
		}
		if c.Dup {
			updates[c.ID] = "skipped" // 已存在：标记跳过，不重复入库
			continue
		}
		it := applyItem{cand: c}
		if c.Target == "live" {
			liveItems = append(liveItems, it)
		} else {
			sourceItems = append(sourceItems, it)
		}
	}

	addedCount := 0
	skippedCount := len(updates)
	var skipped []map[string]string
	var addedSrcs []model.Source
	var addedLiveIDs []string

	// —— TVBox 源候选：导入源列表（逐条：归一化 + 查重 + Add）——
	for _, it := range sourceItems {
		c := it.cand
		norm, err := parser.Normalize(c.URL)
		if err != nil {
			updates[c.ID] = "skipped"
			skippedCount++
			skipped = append(skipped, map[string]string{"url": c.URL, "reason": "地址非法: " + err.Error()})
			continue
		}
		if _, found := a.st.GetByURL(norm); found {
			updates[c.ID] = "skipped"
			skippedCount++
			skipped = append(skipped, map[string]string{"url": norm, "reason": "已存在"})
			continue
		}
		nm := strings.TrimSpace(c.Name)
		if nm == "" {
			nm = parser.HostOf(norm)
		}
		src := model.Source{
			Name: nm, URL: norm, Group: group,
			Enabled: enabled, Type: model.TypeUnknown, Status: model.StatusUnknown,
			Remark: "自动刮削导入",
		}
		if err := a.st.Add(&src); err != nil {
			updates[c.ID] = "skipped"
			skippedCount++
			skipped = append(skipped, map[string]string{"url": norm, "reason": err.Error()})
			continue
		}
		updates[c.ID] = "applied"
		addedCount++
		addedSrcs = append(addedSrcs, src)
	}

	// —— 直播源候选：导入直播源列表（URL 可带 | 请求头参数，按原样入库；store 内去重 + 墓碑恢复）——
	for _, it := range liveItems {
		c := it.cand
		raw := strings.TrimSpace(c.URL)
		if raw == "" {
			updates[c.ID] = "skipped"
			skippedCount++
			continue
		}
		nm := strings.TrimSpace(c.Name)
		ls, err := a.st.AddLiveSource(nm, raw, group, "自动刮削导入", enabled, 0)
		if err != nil {
			updates[c.ID] = "skipped"
			skippedCount++
			skipped = append(skipped, map[string]string{"url": raw, "reason": err.Error()})
			continue
		}
		updates[c.ID] = "applied"
		addedCount++
		addedLiveIDs = append(addedLiveIDs, ls.ID)
	}

	if len(updates) > 0 {
		_ = a.st.UpdateScrapeCandidates(updates)
	}
	// 与批量导入同口径：新增的源立即异步探活，定型后站点源进订阅
	if len(addedSrcs) > 0 {
		go a.asyncCheckSources(addedSrcs)
	}
	// 直播源同理：清失效缓存 + 异步检测
	if len(addedLiveIDs) > 0 {
		a.invalidateLiveCache()
		go a.checkLiveBatch(context.Background(), addedLiveIDs, true)
	}
	if skipped == nil {
		skipped = []map[string]string{}
	}
	ok(w, map[string]interface{}{
		"applied": addedCount, "skipped": skippedCount, "skippedDetail": skipped,
	})
}

// ---- 直播源刮削（A-07 / E-04）----

// looksLikeLivePlaylist 判断内容是否是一整份直播播放列表（m3u / txt 频道表）。
// 特征：以 #EXTM3U 开头；或非 JSON 文本中 http(s) 行数达到阈值（频道表，而非直链清单）。
func looksLikeLivePlaylist(text string) bool {
	t := strings.TrimSpace(strings.TrimPrefix(text, "\ufeff"))
	if t == "" {
		return false
	}
	if strings.HasPrefix(strings.ToLower(t), "#extm3u") {
		return true
	}
	if t[0] == '{' || t[0] == '[' {
		return false
	}
	n := 0
	for _, line := range strings.Split(t, "\n") {
		s := strings.TrimSpace(line)
		if strings.Contains(s, "http://") || strings.Contains(s, "https://") {
			n++
			if n >= 8 {
				return true
			}
		}
	}
	return false
}

// extractLiveSeedEntries 直播种子内容 → 播放列表级候选条目。
//   - 整份播放列表（m3u/txt 频道表）→ 候选为种子自身（导入后作为一条直播源）；
//   - JSON（聚合/清单/lives/urls 等结构）→ 提取其中的播放列表直链；
//   - 少量 URL 行的 txt 清单 → 逐条候选。
func extractLiveSeedEntries(text, seedName, seedURL string) ([]jsonEntry, string) {
	text = strings.TrimSpace(strings.TrimPrefix(text, "\ufeff"))
	if text == "" {
		return nil, ""
	}
	if looksLikeLivePlaylist(text) {
		return []jsonEntry{{Name: seedName, URL: seedURL}}, "playlist"
	}
	if jsonLooksLike(text) {
		if entries := extractLiveURLsFromJSON(text); len(entries) > 0 {
			return entries, "live-json"
		}
	}
	var out []jsonEntry
	seen := map[string]bool{}
	for _, line := range strings.Split(text, "\n") {
		n, u := splitNameURL(line)
		if u == "" || !looksLikeURL(u) || seen[strings.ToLower(u)] {
			continue
		}
		seen[strings.ToLower(u)] = true
		out = append(out, jsonEntry{Name: n, URL: u})
	}
	if len(out) > 0 {
		return out, "live-txt"
	}
	return nil, ""
}

// isPlausibleLiveURL 播放列表直链粗筛：http(s)、非 jar/js/py 等可执行/脚本、非 CMS 接口。
func isPlausibleLiveURL(u string) bool {
	stem, _ := splitLiveHeaderParam(u)
	low := strings.ToLower(stem)
	if !strings.HasPrefix(low, "http://") && !strings.HasPrefix(low, "https://") {
		return false
	}
	for _, bad := range []string{".jar", ".js", ".py", ".apk", ".exe"} {
		if strings.HasSuffix(low, bad) {
			return false
		}
	}
	if strings.Contains(low, "/api.php/") {
		return false
	}
	return true
}

// splitLiveHeaderParam 拆掉 | 之后的请求头参数（与 internal/live.SplitHeader 语义一致）
func splitLiveHeaderParam(raw string) (string, string) {
	if i := strings.Index(raw, "|"); i >= 0 {
		return strings.TrimSpace(raw[:i]), strings.TrimSpace(raw[i+1:])
	}
	return strings.TrimSpace(raw), ""
}

// liveURLScanner JSON 扫描器：只收集 {name,url} 形态的 http(s) 直链。
// 不做 jsonSkipKeys 黑名单——直播 JSON 常见 lives 键；深度 ≤ 8。
type liveURLScanner struct {
	entries []jsonEntry
	seen    map[string]bool
}

func (sc *liveURLScanner) add(name, rawURL string) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" || !isPlausibleLiveURL(rawURL) {
		return
	}
	key := strings.ToLower(rawURL)
	if sc.seen[key] {
		return
	}
	sc.seen[key] = true
	sc.entries = append(sc.entries, jsonEntry{Name: strings.TrimSpace(name), URL: rawURL})
}

func (sc *liveURLScanner) walk(v interface{}, depth int) {
	if depth > 8 {
		return
	}
	switch t := v.(type) {
	case string:
		if isPlausibleLiveURL(t) {
			sc.add("", t)
		}
	case []interface{}:
		for _, x := range t {
			sc.walk(x, depth+1)
		}
	case map[string]interface{}:
		// 1. 自身即条目（含 url 字段）
		if u, ok := t["url"].(string); ok && strings.TrimSpace(u) != "" {
			name := ""
			for _, k := range []string{"name", "title", "remark", "group", "channel"} {
				if n, ok := t[k].(string); ok && strings.TrimSpace(n) != "" {
					name = n
					break
				}
			}
			sc.add(name, u)
			return // 有 url 的对象不再下钻，避免抓取深层无关地址
		}
		// 2. 常见包装键下钻（lives 不过滤）
		for _, k := range []string{"urls", "lives", "data", "list", "items", "result", "rows", "content", "playlist", "channels"} {
			if child, ok := t[k]; ok {
				sc.walk(child, depth+1)
				return
			}
		}
		// 3. 兜底：扫所有子值（字符串直链由 string 分支收集）
		for _, child := range t {
			sc.walk(child, depth+1)
		}
	}
}

// extractLiveURLsFromJSON 从 JSON 文本提取播放列表直链（清洗 JSONC 后扫描）
func extractLiveURLsFromJSON(text string) []jsonEntry {
	cleaned := string(parser.CleanJSON([]byte(text)))
	if !json.Valid([]byte(cleaned)) {
		return nil
	}
	var root interface{}
	if err := json.Unmarshal([]byte(cleaned), &root); err != nil {
		return nil
	}
	sc := &liveURLScanner{seen: map[string]bool{}}
	sc.walk(root, 0)
	return sc.entries
}
