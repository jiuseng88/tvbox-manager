package api

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
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
「源自动刮削」（PRD A-06：TVBox 源 / A-07：直播源，L1+L2 手动闭环）：

  - 手动收录（L1）：填一个订阅地址 → 服务端拉取（复用 checker.Fetch，SSRF 强制开启）
    → 走与「粘贴/文件导入」同一套智能识别（单仓托管 / 多仓提子仓 / 地址列表 / txt）
    → dryRun 预览去重，或直接批量导入。
  - 种子同步（L2）：维护种子清单（源集合站点 / 公开仓库直链），一次同步把每个种子的
    候选条目写入候选区；候选经用户确认后才导入为源（先候选后确认，不自动入库）。
  - 发现引擎：对内容自动嗅探类型（single/multi/list/m3u/json/mixed），不再依赖用户手标
    tvbox/live；受控递归（Depth，默认 1，上限 2）展开多仓；fetch 带同主机限频与重试退避。
  - 富集+评估：发现后顺手解析摘要（站点数/频道数）并算质量分，仅用于排序标注，不阻断导入。
  - 对账（重建核心）：候选 ID 由 (SeedID+URL) 派生、upsert 合并——已确认状态不被整批覆盖；
    上游已移除的已导入源标记 UpstreamRemoved（漂移，不自动删除），可进复核。
  - 合规约束：来源仅限用户主动添加的种子 URL；全链路 SSRF 防护；同步不自动删除已有源；
    已存在（dup）的候选标记出来但默认不重复入库。

    IPTV 源（A-07）与 TVBox 源共用同一套「同步 → 候选 → 确认」闭环，target 由嗅探决定。
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
	Type    string `json:"type"` // tvbox（倾向，默认）/ live（倾向）；实际 target 由嗅探决定
	Group   string `json:"group"`
	Enabled *bool  `json:"enabled"`
	Remark  string `json:"remark"`
	Depth   int    `json:"depth"` // 受控递归深度，默认 1，上限 2
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
	en := true
	if req.Enabled != nil {
		en = *req.Enabled
	}
	depth := req.Depth
	if depth <= 0 {
		depth = 1
	}
	if depth > 2 {
		depth = 2
	}
	created, err := a.st.AddScrapeSeed(&model.ScrapeSeed{
		Name: name, URL: norm, Type: stype, Group: group, Enabled: en, Remark: strings.TrimSpace(req.Remark), Depth: depth,
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
	Type    *string `json:"type"`
	Group   *string `json:"group"`
	Enabled *bool   `json:"enabled"`
	Remark  *string `json:"remark"`
	Depth   *int    `json:"depth"`
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
			st := strings.TrimSpace(*req.Type)
			if st == "tvbox" || st == "live" {
				sp.Type = st
			}
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
		if req.Depth != nil {
			d := *req.Depth
			if d <= 0 {
				d = 1
			}
			if d > 2 {
				d = 2
			}
			sp.Depth = d
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
// 手动触发一轮种子同步（与定时刮削共用 runScrapeSync）。
func (a *API) handleScrapeSync(w http.ResponseWriter, r *http.Request) {
	stat := a.runScrapeSync(r.Context())
	if stat == nil {
		writeJSON(w, http.StatusConflict, envelope{Code: 409, Msg: "已有同步任务在运行"})
		return
	}
	ok(w, stat)
}

// runScrapeSync 遍历启用的种子，逐个发现 → 归一化去重 → upsert 候选区（不自动入库）→ 漂移对账。
// HTTP 手动触发与定时调度（scheduler.SetScrapeSync）共用；同一时刻只允许一个同步任务，
// 被占用时返回 nil。调用方应基于返回的统计展示/记录。
func (a *API) runScrapeSync(ctx context.Context) *scrapeSyncStat {
	if !scrapeSyncMu.TryLock() {
		return nil
	}
	defer scrapeSyncMu.Unlock()

	seeds := a.st.ListScrapeSeeds()
	stat := &scrapeSyncStat{}
	now := time.Now()
	hl := newHostLimiter(150 * time.Millisecond)

	for i := range seeds {
		sd := seeds[i]
		if !sd.Enabled {
			continue
		}
		stat.Seeds++

		items, msg := a.discoverSeed(ctx, sd, hl)
		if msg != "" {
			stat.Errors = append(stat.Errors, fmt.Sprintf("%s：%s", sd.Name, msg))
			a.st.TouchScrapeSeedSync(sd.ID, msg, now)
			continue
		}

		// 归一化 + 去重 + 标记 dup + 建候选
		var cands []model.ScrapeCandidate
		seen := map[string]bool{}
		var srcURLs []string
		var liveURLs []string
		for _, it := range items {
			storeURL := strings.TrimSpace(it.URL)
			if norm, nerr := parser.Normalize(storeURL); nerr == nil {
				storeURL = norm
			}
			key := strings.ToLower(storeURL)
			if storeURL == "" || seen[key] {
				continue
			}
			seen[key] = true

			var exists bool
			if it.Target == "live" {
				exists = a.st.LiveSourceExists(storeURL)
				liveURLs = append(liveURLs, storeURL)
			} else {
				_, exists = a.st.GetByURL(storeURL)
				srcURLs = append(srcURLs, storeURL)
			}
			cands = append(cands, model.ScrapeCandidate{
				SeedID:         sd.ID,
				SeedName:       sd.Name,
				Name:           it.Name,
				URL:            storeURL,
				Target:         it.Target,
				Kind:           it.Kind,
				Dup:            exists,
				DiscoveredDepth: it.Depth,
				ParentURL:      it.ParentURL,
				Quality:        it.Quality,
				Summary:        it.Summary,
				FP:             it.FP,
				FetchedAt:      now,
				LastSeenAt:     now,
			})
		}

		if len(cands) == 0 {
			a.st.TouchScrapeSeedSync(sd.ID, "同步完成，未识别到任何源地址", now)
			continue
		}
		if err := a.st.UpsertScrapeCandidates(sd.ID, cands); err != nil {
			stat.Errors = append(stat.Errors, fmt.Sprintf("%s：候选写入失败: %v", sd.Name, err))
			a.st.TouchScrapeSeedSync(sd.ID, "候选写入失败: "+err.Error(), now)
			continue
		}

		// 漂移对账：已导入源 vs 本次发现集（上游移除 → 仅标记，不删）
		removed, _ := a.st.ReconcileDrift(sd.ID, srcURLs, liveURLs)

		stat.Synced++
		stat.Candidates += len(cands)
		dup := 0
		for _, c := range cands {
			if c.Dup {
				dup++
			}
		}
		stat.Duplicates += dup
		msg = fmt.Sprintf("同步完成：识别 %d 条候选（%d 条已存在）", len(cands), dup)
		if removed > 0 {
			msg += fmt.Sprintf("，%d 个已导入源上游失效", removed)
		}
		a.st.TouchScrapeSeedSync(sd.ID, msg, now)
	}

	// 末次 Save 落盘所有种子的同步结果（候选/漂移已各自 flush）
	if err := a.st.Save(); err != nil {
		log.Printf("[scrape] 同步后落盘失败: %v", err)
	}
	return stat
}

// ---- 发现引擎 ----

type discoveredItem struct {
	Name, URL string
	Target   string // source / live
	Kind     string // repo-single / repo-multi / urls / array / txt / playlist / live-json / live-txt
	Depth    int    // 发现深度：0=种子自身内容，1+=递归层级
	ParentURL string
	Quality  int // 0~100，仅排序
	Summary  string
	FP       string // 上游内容指纹
}

// discoverSeed 抓取种子并发现条目（顶层，depth 来自种子配置）。
func (a *API) discoverSeed(ctx context.Context, seed model.ScrapeSeed, hl *hostLimiter) ([]discoveredItem, string) {
	depth := seed.Depth
	if depth <= 0 {
		depth = 1
	}
	if depth > 2 {
		depth = 2
	}
	res, body := a.sch.Checker().Fetch(ctx, seed.URL, true) // SSRF 强制
	if res.Status != model.StatusOK {
		return nil, "同步失败: " + res.Msg
	}
	items, err := a.discoverContent(ctx, string(body), seed.Name, seed.URL, depth, 0, "", fingerprint(body), seed.Type == "live", hl)
	if err != nil {
		return nil, err.Error()
	}
	return items, ""
}

// discoverContent 内容识别核心（纯文本 → 条目列表）。递归展开多仓时按 depth 预算收敛。
// 类型自动嗅探：m3u/频道表 → 直播；TVBox 仓库 → 源；地址列表/txt 逐条按 entryTarget 归类；
// 模糊项按种子意图（preferLive）兜底。不再强依赖用户手标 tvbox/live。
func (a *API) discoverContent(ctx context.Context, text, seedName, baseURLStr string, depth, curDepth int, parentURL, baseFP string, preferLive bool, hl *hostLimiter) ([]discoveredItem, error) {
	text = strings.TrimSpace(strings.TrimPrefix(text, "\ufeff"))
	if text == "" {
		return nil, fmt.Errorf("内容为空")
	}

	// 1. 直播播放列表（m3u / 频道表）：整份作为一条直播候选
	if looksLikeLivePlaylist(text) {
		return []discoveredItem{{
			Name: seedName, URL: baseURLStr, Target: "live", Kind: "playlist", Depth: curDepth,
			ParentURL: parentURL, Quality: 75, Summary: summarizeLive(text), FP: baseFP,
		}}, nil
	}

	cleaned := string(parser.CleanJSON([]byte(text)))

	// 2. TVBox 仓库（含 sites/lives/parses）→ 作为单仓/多仓源候选
	if typ, detail, err := parser.Parse([]byte(cleaned)); err == nil {
		switch typ {
		case model.TypeMulti:
			if preferLive {
				// 直播意图下：多仓子地址按 live 意图逐条归类（脚本/接口直链丢弃，仅留播放列表直链），不再当子仓递归展开。
				items := make([]discoveredItem, 0, len(detail.SubURLs))
				for _, s := range detail.SubURLs {
					u := strings.TrimSpace(s.URL)
					if u == "" {
						continue
					}
					if entryTarget(u, "live") != "live" {
						continue
					}
					items = append(items, discoveredItem{
						Name: s.Name, URL: u, Target: "live", Kind: "live-json", Depth: curDepth,
						ParentURL: parentURL, Quality: 60,
					})
				}
				if len(items) > 0 {
					return items, nil
				}
				return nil, fmt.Errorf("多仓直播意图下未识别到播放列表直链")
			}
			return a.expandMulti(ctx, detail.SubURLs, depth, curDepth, baseURLStr, baseFP, hl)
		case model.TypeSingle:
			// lives-only 的 JSON 在 live 意图下当作「播放列表集合」提取，而非单仓源
			if !(preferLive && detail.Sites == 0) {
				return []discoveredItem{{
					Name: seedName, URL: baseURLStr, Target: "source", Kind: "repo-single", Depth: curDepth,
					ParentURL: parentURL, Quality: qualityFromDetail(detail), Summary: summarizeDetail(detail), FP: baseFP,
				}}, nil
			}
		}
	}

	// 未识别为仓库时，由种子倾向决定意图（tvbox→source / live→live）
	intent := "source"
	if preferLive {
		intent = "live"
	}

	// 3. 直播集合 JSON（仅含 lives / 播放列表直链）：提取为直播候选
	if jsonLooksLike(text) {
		var probe map[string]interface{}
		if json.Unmarshal([]byte(cleaned), &probe) == nil {
			if _, hasLives := probe["lives"]; hasLives {
				if entries := extractLiveURLsFromJSON(text); len(entries) > 0 {
					items := make([]discoveredItem, 0, len(entries))
					for _, e := range entries {
						items = append(items, discoveredItem{Name: e.Name, URL: e.URL, Target: "live", Kind: "live-json", Depth: curDepth, ParentURL: parentURL, Quality: 60})
					}
					return items, nil
				}
			}
		}
	}

	// 4. JSON 地址列表（分享格式 / 数组 / 备份 / 深扫）→ 逐条按意图归类
	if parsed, perr := ParseSourceJSON(cleaned); perr == nil && len(parsed.Entries) > 0 {
		items := make([]discoveredItem, 0, len(parsed.Entries))
		for _, e := range parsed.Entries {
			t := entryTarget(e.URL, intent)
			if t == "" {
				continue
			}
			items = append(items, discoveredItem{Name: e.Name, URL: e.URL, Target: t, Kind: parsed.Kind, Depth: curDepth, ParentURL: parentURL, Quality: 60})
		}
		if len(items) > 0 {
			return items, nil
		}
		return nil, fmt.Errorf("地址列表未识别到有效条目")
	}

	// 5. txt 行列表兜底（只接受 http(s) 开头行）
	var out []discoveredItem
	seen := map[string]bool{}
	for _, line := range strings.Split(text, "\n") {
		n, u := splitNameURL(line)
		if u == "" || !looksLikeURL(u) || seen[strings.ToLower(u)] {
			continue
		}
		seen[strings.ToLower(u)] = true
		t := entryTarget(u, intent)
		if t == "" {
			continue
		}
		kind := "txt"
		if t == "live" {
			kind = "live-txt"
		}
		out = append(out, discoveredItem{Name: n, URL: u, Target: t, Kind: kind, Depth: curDepth, ParentURL: parentURL, Quality: 50})
	}
	if len(out) > 0 {
		return out, nil
	}
	return nil, fmt.Errorf("未识别到任何源地址")
}

// entryTarget 单条地址归类：脚本/二进制直接丢弃；m3u 类→直播；json/CMS→源；
// 模糊（.txt/无扩展名/裸域名）按种子意图兜底。返回 "" 表示丢弃。
func entryTarget(u, intent string) string {
	low := strings.ToLower(strings.TrimSpace(u))
	for _, bad := range []string{".jar", ".js", ".py", ".apk", ".exe"} {
		if strings.HasSuffix(low, bad) {
			return ""
		}
	}
	if strings.HasSuffix(low, ".m3u") || strings.HasSuffix(low, ".m3u8") {
		return "live"
	}
	if strings.HasSuffix(low, ".json") || strings.Contains(low, "/api.php") {
		return "source"
	}
	if intent == "live" {
		return "live"
	}
	return "source"
}

// expandMulti 多仓：提取子仓地址为源候选；若 depth 允许且子仓本身仍是多仓，则递归展开一层。
func (a *API) expandMulti(ctx context.Context, subURLs []model.SubURL, depth, curDepth int, parentURL, baseFP string, hl *hostLimiter) ([]discoveredItem, error) {
	out := make([]discoveredItem, 0, len(subURLs))
	seen := map[string]bool{}
	for _, s := range subURLs {
		u := strings.TrimSpace(s.URL)
		if u == "" || seen[strings.ToLower(u)] {
			continue
		}
		seen[strings.ToLower(u)] = true

		if curDepth+1 < depth {
			// 受控递归：抓取子仓看是否还是多仓
			if hl != nil {
				hl.wait(parser.HostOf(u))
			}
			res, body := a.sch.Checker().Fetch(ctx, u, true)
			if res.Status == model.StatusOK {
				child, _ := a.discoverContent(ctx, string(body), s.Name, u, depth, curDepth+1, u, fingerprint(body), false, hl)
				out = append(out, child...)
				continue
			}
			// 抓取失败：仍作为源候选（地址本身），待确认后由探活判定
		}
		out = append(out, discoveredItem{
			Name: s.Name, URL: u, Target: "source", Kind: "repo-multi", Depth: curDepth + 1,
			ParentURL: parentURL, Quality: 55,
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("多仓配置中未提取到子仓地址")
	}
	return out, nil
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
// 复用批量导入语义（归一化 + 查重 + 异步探活）；导入结果回写候选状态：
// 成功 → applied；已存在/非法 → skipped。全程单次 flush，避免逐条写盘。
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

	var srcItems []model.Source
	var liveItems []store.LiveAddItem
	updates := map[string]string{}
	skipped := 0
	now := time.Now()

	for _, c := range a.st.ListScrapeCandidates() {
		if !want[c.ID] {
			continue
		}
		if c.Status != "pending" {
			updates[c.ID] = "skipped"
			skipped++
			continue
		}
		if c.Dup {
			updates[c.ID] = "skipped"
			skipped++
			continue
		}
		if c.Target == "live" {
			raw := strings.TrimSpace(c.URL)
			if raw == "" {
				updates[c.ID] = "skipped"
				skipped++
				continue
			}
			liveItems = append(liveItems, store.LiveAddItem{
				Name: c.Name, RawURL: raw, Group: group, Remark: "自动刮削导入",
				Enabled: enabled, SeedID: c.SeedID, DiscoveredAt: &now, UpstreamFP: c.FP,
			})
			updates[c.ID] = "applied"
			continue
		}
		// source
		norm, err := parser.Normalize(c.URL)
		if err != nil {
			updates[c.ID] = "skipped"
			skipped++
			continue
		}
		if _, found := a.st.GetByURL(norm); found {
			updates[c.ID] = "skipped"
			skipped++
			continue
		}
		nm := strings.TrimSpace(c.Name)
		if nm == "" {
			nm = parser.HostOf(norm)
		}
		srcItems = append(srcItems, model.Source{
			Name: nm, URL: norm, Group: group, Enabled: enabled,
			Type: model.TypeUnknown, Status: model.StatusUnknown, Remark: "自动刮削导入",
			SeedID: c.SeedID, DiscoveredAt: &now, UpstreamFP: c.FP,
		})
		updates[c.ID] = "applied"
	}

	addedSrc, err := a.st.AddManySources(srcItems)
	if err != nil {
		serverError(w, err)
		return
	}
	addedLiveIDs, _, err2 := a.st.AddManyLiveSources(liveItems)
	if err2 != nil {
		serverError(w, err2)
		return
	}

	if len(updates) > 0 {
		_ = a.st.UpdateScrapeCandidates(updates)
	}

	applied := addedSrc + len(addedLiveIDs)
	// 与批量导入同口径：新增的源立即异步探活，定型后站点源进订阅
	if len(srcItems) > 0 {
		go a.asyncCheckSources(srcItems)
	}
	if len(addedLiveIDs) > 0 {
		a.invalidateLiveCache()
		go a.checkLiveBatch(context.Background(), addedLiveIDs, true)
	}

	ok(w, map[string]interface{}{
		"applied": applied, "skipped": skipped,
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

// ---- 工具 ----

// hostLimiter 同主机最小间隔限频（递归抓取子仓时避免打爆同一 CDN）。
type hostLimiter struct {
	mu   sync.Mutex
	last map[string]time.Time
	min  time.Duration
}

func newHostLimiter(min time.Duration) *hostLimiter {
	return &hostLimiter{last: map[string]time.Time{}, min: min}
}

func (h *hostLimiter) wait(host string) {
	h.mu.Lock()
	prev, ok := h.last[host]
	h.mu.Unlock()
	if ok {
		if d := time.Since(prev); d < h.min {
			time.Sleep(h.min - d)
		}
	}
	h.mu.Lock()
	h.last[host] = time.Now()
	h.mu.Unlock()
}

// fingerprint 上游内容指纹（sha1 前 12 位），用于内容级漂移比对。
func fingerprint(body []byte) string {
	h := sha1.Sum(body)
	return hex.EncodeToString(h[:])[:12]
}

// qualityFromDetail 由仓库结构丰富度给质量分（仅排序信号）。
func qualityFromDetail(d *model.Detail) int {
	q := 60
	if d.Sites > 0 {
		q += 25
	}
	if d.Lives > 0 {
		q += 5
	}
	if q > 100 {
		q = 100
	}
	return q
}

// summarizeDetail 仓库摘要：站点/直播数。
func summarizeDetail(d *model.Detail) string {
	return fmt.Sprintf("%d 站点 / %d 直播", d.Sites, d.Lives)
}

// summarizeLive 直播播放列表摘要：频道数（#EXTINF 计数）。
func summarizeLive(text string) string {
	n := 0
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#EXTINF") {
			n++
		}
	}
	return fmt.Sprintf("%d 频道", n)
}
