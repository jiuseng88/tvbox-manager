package api

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"tvbox-manager/internal/checker"
	"tvbox-manager/internal/live"
	"tvbox-manager/internal/model"
	"tvbox-manager/internal/store"
)

// 直播源管理（管理端，需 Token）。
//
// 检测分两层：
//   - 源级：能不能拉到、解析出多少频道 / 多少分组、HTTP 码、耗时；
//   - 频道抽样：按分组均匀抽 N 个频道，拉 2KB 判断能不能播，得到可用率。
//
// 全量频道探测能准，但一个大源上千频道、几十个源就是几万个请求，必然触发上游限流，
// 所以只保留「抽样」这一档，不做定时全量。

const (
	liveSampleLimit  = 20              // 每源抽样频道数
	liveProbeWorkers = 8               // 频道探测并发
	liveProbeDoor    = 6 * time.Second // 单频道探测超时
	liveProbeMaxBody = 4096            // 只读 4KB 判魔数
	liveCheckWorkers = 3               // 批量检测源之间的并发
	liveDefaultDoor  = 12              // 拉播放列表的默认超时（秒）
	liveRangeHeader  = "bytes=0-4095"
)

var liveProbeClient = &http.Client{
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           netDialer.DialContext,
		ResponseHeaderTimeout: 8 * time.Second,
		TLSHandshakeTimeout:   8 * time.Second,
		IdleConnTimeout:       60 * time.Second,
		MaxIdleConnsPerHost:   8,
	},
	CheckRedirect: tvCheckRedirect,
}

// ---- 列表 ----

func (a *API) handleLiveSourceList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.LiveFilter{
		Keyword: q.Get("keyword"),
		Status:  q.Get("status"),
		Group:   q.Get("group"),
		Enabled: q.Get("enabled"),
		Origin:  q.Get("origin"),
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
	list, total := a.st.ListLiveSourcesPage(f)
	if list == nil {
		list = []model.LiveSource{}
	}
	ok(w, map[string]interface{}{
		"list":  list,
		"total": total,
		"page":  f.Page,
		"size":  f.Size,
	})
}

func (a *API) handleLiveSourceGroups(w http.ResponseWriter, r *http.Request) {
	ok(w, a.st.LiveGroups())
}

// ---- 新增 ----

type liveAddReq struct {
	Name           string `json:"name"`
	URL            string `json:"url"`
	Group          string `json:"group"`
	Remark         string `json:"remark"`
	Enabled        *bool  `json:"enabled"`
	TimeoutSeconds int    `json:"timeoutSeconds"`
	Check          *bool  `json:"check"` // 添加后是否立刻检测，默认 true
}

func (a *API) handleLiveSourceAdd(w http.ResponseWriter, r *http.Request) {
	var req liveAddReq
	if !decodeJSON(w, r, &req) {
		return
	}
	raw, err := normalizeLiveURL(req.URL)
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	ls, err := a.st.AddLiveSource(req.Name, raw, req.Group, req.Remark, enabled, req.TimeoutSeconds)
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	a.invalidateLiveCache()
	if req.Check == nil || *req.Check {
		go a.checkLiveSource(context.Background(), ls.ID, true)
	}
	ok(w, ls)
}

// ---- 多行批量添加 ----

type liveBatchAddReq struct {
	Text    string `json:"text"`
	Group   string `json:"group"`
	Enabled *bool  `json:"enabled"`
	DryRun  bool   `json:"dryRun"`
	Check   *bool  `json:"check"`
}

func (a *API) handleLiveSourceBatchAdd(w http.ResponseWriter, r *http.Request) {
	var req liveBatchAddReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Text) == "" {
		badRequest(w, "内容为空")
		return
	}
	if len(req.Text) > maxJSONBody {
		badRequest(w, fmt.Sprintf("内容超过 %dMB 上限", maxJSONBody>>20))
		return
	}
	lines := parseLiveLines(req.Text)
	items := make([]model.LiveSource, 0, len(lines))
	group := req.Group
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	for _, ln := range lines {
		if !ln.OK {
			continue
		}
		items = append(items, model.LiveSource{Name: ln.Name, URL: ln.URL, Group: group, Enabled: enabled})
	}
	if req.DryRun {
		ok(w, map[string]interface{}{"lines": lines, "total": len(lines), "valid": len(items)})
		return
	}
	if len(items) == 0 {
		badRequest(w, "没有可添加的有效行（格式见输入框提示）")
		return
	}
	added, skipped := a.st.AddLiveSources(items)
	a.invalidateLiveCache()
	if len(added) > 0 && (req.Check == nil || *req.Check) {
		go a.checkLiveBatch(context.Background(), added, true)
	}
	ok(w, map[string]interface{}{
		"added":   len(added),
		"skipped": skipped,
		"lines":   lines,
	})
}

// liveLine 一行解析结果
type liveLine struct {
	Line   int    `json:"line"`
	Raw    string `json:"raw"`
	Name   string `json:"name"`
	URL    string `json:"url"`
	OK     bool   `json:"ok"`
	Reason string `json:"reason,omitempty"`
}

// parseLiveLines 解析多行直播源文本。支持的写法（混排）：
//
//	名称,地址          ← 首选，中英文逗号都认
//	名称|地址          ← | 后面必须紧跟 http，否则那是地址里的请求头参数
//	名称 地址          ← 空白分隔（名称不含空格）
//	地址               ← 单地址，名称取主机名
//
// 空行、# 开头的注释行忽略。
func parseLiveLines(text string) []liveLine {
	raw := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	out := make([]liveLine, 0, len(raw))
	for i, line := range raw {
		s := strings.TrimSpace(strings.Trim(line, "\u200b\u200c\u200d\u2060\ufeff\u00a0"))
		if s == "" || (strings.HasPrefix(s, "#") && !strings.HasPrefix(strings.ToLower(s), "#extm3u")) {
			continue
		}
		ln := liveLine{Line: i + 1, Raw: s}
		name, addr, reason := splitLiveLine(s)
		if reason != "" {
			ln.Reason = reason
			out = append(out, ln)
			continue
		}
		norm, err := normalizeLiveURL(addr)
		if err != nil {
			ln.Reason = err.Error()
			out = append(out, ln)
			continue
		}
		ln.Name = name
		ln.URL = norm
		ln.OK = true
		out = append(out, ln)
	}
	return out
}

func splitLiveLine(s string) (name, addr, reason string) {
	// 1) 逗号：名称,地址
	if i := strings.IndexAny(s, ",，"); i > 0 {
		name = strings.TrimSpace(s[:i])
		addr = strings.TrimSpace(s[i+1:])
		if strings.HasPrefix(strings.ToLower(addr), "http") {
			return name, addr, ""
		}
	}
	// 2) 竖线：名称|http...  —— | 后必须是 http，否则属于地址里的请求头参数
	if i := strings.Index(s, "|"); i > 0 {
		rest := strings.TrimSpace(s[i+1:])
		if strings.HasPrefix(strings.ToLower(rest), "http") {
			return strings.TrimSpace(s[:i]), rest, ""
		}
	}
	// 3) 空白：名称 地址
	if i := strings.IndexAny(s, " \t"); i > 0 {
		head := strings.TrimSpace(s[:i])
		tail := strings.TrimSpace(s[i+1:])
		if strings.HasPrefix(strings.ToLower(tail), "http") && !strings.ContainsAny(head, " \t") {
			return head, tail, ""
		}
	}
	// 4) 纯地址
	if strings.HasPrefix(strings.ToLower(s), "http") {
		return "", s, ""
	}
	return "", "", "既不是 http 地址，也不是「名称,地址」格式"
}

// normalizeLiveURL 校验并归一化：只接受 http/https，剥空白，保留 | 请求头参数。
func normalizeLiveURL(raw string) (string, error) {
	raw = strings.TrimSpace(strings.Trim(raw, "\u200b\u200c\u200d\u2060\ufeff\u00a0"))
	if raw == "" {
		return "", fmt.Errorf("地址为空")
	}
	stem, header := live.SplitHeader(raw)
	u, err := url.Parse(stem)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("地址格式不对：%s", stem)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("仅支持 http/https 地址（%s 需要专用播放器）", u.Scheme)
	}
	if header != "" {
		return stem + "|" + header, nil
	}
	return stem, nil
}

// ---- 修改 / 删除 ----

type liveUpdateReq struct {
	Name           *string `json:"name"`
	URL            *string `json:"url"`
	Group          *string `json:"group"`
	Remark         *string `json:"remark"`
	Enabled        *bool   `json:"enabled"`
	Sort           *int    `json:"sort"`
	TimeoutSeconds *int    `json:"timeoutSeconds"`
}

func (a *API) handleLiveSourceUpdate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, found := a.st.GetLiveSource(id); !found {
		notFound(w, "直播源不存在")
		return
	}
	var req liveUpdateReq
	if !decodeJSON(w, r, &req) {
		return
	}
	var normURL string
	if req.URL != nil {
		var err error
		normURL, err = normalizeLiveURL(*req.URL)
		if err != nil {
			badRequest(w, err.Error())
			return
		}
	}
	err := a.st.UpdateLiveSource(id, func(ls *model.LiveSource) {
		if req.Name != nil {
			ls.Name = strings.TrimSpace(*req.Name)
			ls.NameAuto = false // 手动改名后不再随父源同步
		}
		if req.Group != nil {
			ls.Group = strings.TrimSpace(*req.Group)
			if ls.Group == "" {
				ls.Group = "默认"
			}
			ls.GroupAuto = false
		}
		if req.Remark != nil {
			ls.Remark = strings.TrimSpace(*req.Remark)
		}
		if req.Enabled != nil {
			ls.Enabled = *req.Enabled
		}
		if req.Sort != nil {
			ls.Sort = *req.Sort
		}
		if req.TimeoutSeconds != nil {
			ls.TimeoutSeconds = *req.TimeoutSeconds
		}
		if normURL != "" && normURL != ls.URL {
			// 换地址等于换了个源：旧的频道数和检测结论都不能留着
			ls.URL = normURL
			ls.Status = model.StatusUnknown
			ls.CheckMsg = ""
			ls.HTTPCode = 0
			ls.LatencyMs = 0
			ls.ChannelCount = 0
			ls.GroupCount = 0
			ls.AliveCount = 0
			ls.SampleCount = 0
			ls.FailStreak = 0
			ls.OKStreak = 0
		}
	})
	if err != nil {
		serverError(w, err)
		return
	}
	a.invalidateLiveCache()
	ls, _ := a.st.GetLiveSource(id)
	ok(w, ls)
}

func (a *API) handleLiveSourceDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, found := a.st.GetLiveSource(id); !found {
		notFound(w, "直播源不存在")
		return
	}
	n := a.st.DeleteLiveSources([]string{id})
	a.invalidateLiveCache()
	ok(w, map[string]int{"deleted": n})
}

func (a *API) handleLiveSourceToggle(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ls, found := a.st.GetLiveSource(id)
	if !found {
		notFound(w, "直播源不存在")
		return
	}
	want := !ls.Enabled
	if err := a.st.UpdateLiveSource(id, func(x *model.LiveSource) { x.Enabled = want }); err != nil {
		serverError(w, err)
		return
	}
	a.invalidateLiveCache()
	ok(w, map[string]bool{"enabled": want})
}

type liveBatchActionReq struct {
	IDs    []string `json:"ids"`
	Action string   `json:"action"` // enable / disable / delete / group
	Group  string   `json:"group"`
}

func (a *API) handleLiveSourceBatchAction(w http.ResponseWriter, r *http.Request) {
	var req liveBatchActionReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if len(req.IDs) == 0 {
		badRequest(w, "未选择任何条目")
		return
	}
	n, err := a.st.BatchLiveSources(req.IDs, req.Action, req.Group)
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	a.invalidateLiveCache()
	ok(w, map[string]int{"affected": n})
}

// ---- 检测 ----

func (a *API) handleLiveSourceCheck(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, found := a.st.GetLiveSource(id); !found {
		notFound(w, "直播源不存在")
		return
	}
	go a.checkLiveSource(context.Background(), id, true)
	ok(w, map[string]bool{"started": true})
}

type liveBatchCheckReq struct {
	IDs     []string `json:"ids"`
	All     bool     `json:"all"`
	Keyword string   `json:"keyword"`
}

func (a *API) handleLiveSourceBatchCheck(w http.ResponseWriter, r *http.Request) {
	var req liveBatchCheckReq
	if !decodeJSON(w, r, &req) {
		return
	}
	var ids []string
	if req.All {
		list, _ := a.st.ListLiveSourcesPage(store.LiveFilter{Keyword: req.Keyword, Size: maxPageSize, Page: 1})
		for _, ls := range list {
			ids = append(ids, ls.ID)
		}
	} else {
		ids = req.IDs
	}
	if len(ids) == 0 {
		badRequest(w, "未选择任何条目")
		return
	}
	go a.checkLiveBatch(context.Background(), ids, true)
	ok(w, map[string]int{"started": len(ids)})
}

// checkLiveBatch 批量检测：源之间并发 liveCheckWorkers，避免几十个源同时打满上游。
func (a *API) checkLiveBatch(ctx context.Context, ids []string, persist bool) {
	if len(ids) == 0 {
		return
	}
	sem := make(chan struct{}, liveCheckWorkers)
	done := make(chan struct{}, len(ids))
	for _, id := range ids {
		sem <- struct{}{}
		go func(id string) {
			defer func() { <-sem; done <- struct{}{} }()
			a.checkLiveSource(ctx, id, persist)
		}(id)
	}
	for range ids {
		<-done
	}
}

// checkLiveSource 单个直播源的完整检测：拉列表 → 解析 → 抽样探频道 → 落库。
func (a *API) checkLiveSource(ctx context.Context, id string, persist bool) {
	ls, found := a.st.GetLiveSource(id)
	if !found {
		return
	}
	a.st.SetLiveChecking(id)

	timeout := ls.TimeoutSeconds
	if timeout <= 0 {
		timeout = liveDefaultDoor
	}
	start := time.Now()
	chans, code, err := fetchLivePlaylistCtx(ctx, ls.URL, ls.Name, time.Duration(timeout)*time.Second)
	latency := time.Since(start).Milliseconds()

	res := store.LiveCheckResult{HTTPCode: code, LatencyMs: latency}
	if err != nil {
		res.Status = model.StatusFail
		res.Msg = err.Error()
		_ = a.st.UpdateLiveCheckResult(id, res, persist)
		a.invalidateLiveCache()
		return
	}

	groups := make(map[string]bool, 16)
	for _, ch := range chans {
		groups[ch.Group] = true
	}
	res.ChannelCount = len(chans)
	res.GroupCount = len(groups)

	if len(chans) == 0 {
		res.Status = model.StatusFail
		res.Msg = "播放列表能拉取，但解析不出可播放频道（可能全是 proxy:// 或 rtmp/rtsp）"
		_ = a.st.UpdateLiveCheckResult(id, res, persist)
		a.invalidateLiveCache()
		return
	}

	samples := pickLiveSamples(chans, liveSampleLimit)
	alive := probeLiveChannels(ctx, samples)
	res.AliveCount = alive
	res.SampleCount = len(samples)

	switch {
	case alive == 0:
		res.Status = model.StatusFail
		res.Msg = fmt.Sprintf("%d 个频道、%d 组，抽样 %d 个全部无法播放", len(chans), len(groups), len(samples))
	case alive < len(samples):
		res.Status = model.StatusOK
		res.Msg = fmt.Sprintf("%d 个频道、%d 组，抽样 %d 个可用 %d 个（部分线路失效）", len(chans), len(groups), len(samples), alive)
	default:
		res.Status = model.StatusOK
		res.Msg = fmt.Sprintf("%d 个频道、%d 组，抽样 %d 个全部可用", len(chans), len(groups), len(samples))
	}
	_ = a.st.UpdateLiveCheckResult(id, res, persist)
	a.invalidateLiveCache()
}

// fetchLivePlaylistCtx 拉取并解析播放列表，返回 HTTP 状态码（0 = 没拿到响应）。
func fetchLivePlaylistCtx(ctx context.Context, rawURL, sourceName string, timeout time.Duration) ([]live.Channel, int, error) {
	stem, hdr := live.SplitHeader(rawURL)
	u, err := url.Parse(stem)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, 0, errBadLiveURL
	}
	if err := checker.RejectPrivateHost(u.Hostname()); err != nil {
		return nil, 0, err
	}

	fctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(fctx, http.MethodGet, stem, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/122.0.0.0 Safari/537.36")
	applyProxyHeaders(req, hdr)

	resp, err := tvLiveClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, resp.StatusCode, &httpStatusError{code: resp.StatusCode}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, tvLiveMaxBody))
	if err != nil && len(body) == 0 {
		return nil, resp.StatusCode, err
	}
	return live.Parse(body, resp.Request.URL, sourceName), resp.StatusCode, nil
}

// pickLiveSamples 按分组轮询取样：每个分组先取一个，再取第二个，直到凑够 limit。
// 这样不会因为前几个分组频道特别多，把抽样全耗在同一个分类里。
func pickLiveSamples(chans []live.Channel, limit int) []live.Channel {
	if limit <= 0 || len(chans) <= limit {
		return chans
	}
	buckets := make(map[string][]live.Channel, 16)
	order := make([]string, 0, 16)
	for _, ch := range chans {
		if _, ok := buckets[ch.Group]; !ok {
			order = append(order, ch.Group)
		}
		buckets[ch.Group] = append(buckets[ch.Group], ch)
	}
	out := make([]live.Channel, 0, limit)
	for len(out) < limit {
		progressed := false
		for _, g := range order {
			if len(buckets[g]) == 0 {
				continue
			}
			out = append(out, buckets[g][0])
			buckets[g] = buckets[g][1:]
			progressed = true
			if len(out) >= limit {
				break
			}
		}
		if !progressed {
			break
		}
	}
	return out
}

// probeLiveChannels 并发探测抽样频道，返回可播放的数量。
func probeLiveChannels(ctx context.Context, chans []live.Channel) int {
	if len(chans) == 0 {
		return 0
	}
	sem := make(chan struct{}, liveProbeWorkers)
	var alive int
	done := make(chan bool, len(chans))
	for _, ch := range chans {
		urls := ch.URLs
		if len(urls) == 0 {
			done <- false
			continue
		}
		sem <- struct{}{}
		go func(raw string) {
			defer func() { <-sem }()
			done <- probeLiveChannel(ctx, raw)
		}(urls[0])
	}
	for range chans {
		if <-done {
			alive++
		}
	}
	return alive
}

// probeLiveChannel 判断一个频道地址能不能播：拉到 4KB 就够——
// HLS 看有没有 #EXTM3U，FLV/MP4 直链看 Content-Type。读满就断，不会把直播流拖住。
func probeLiveChannel(ctx context.Context, raw string) bool {
	stem, hdr := live.SplitHeader(raw)
	u, err := url.Parse(stem)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	if checker.RejectPrivateHost(u.Hostname()) != nil {
		return false
	}
	pctx, cancel := context.WithTimeout(ctx, liveProbeDoor)
	defer cancel()
	req, err := http.NewRequestWithContext(pctx, http.MethodGet, stem, nil)
	if err != nil {
		return false
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/122.0.0.0 Safari/537.36")
	req.Header.Set("Range", liveRangeHeader)
	applyProxyHeaders(req, hdr)

	resp, err := liveProbeClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return false
	}
	head, _ := io.ReadAll(io.LimitReader(resp.Body, liveProbeMaxBody))
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if strings.Contains(ct, "mpegurl") || bytes.Contains(head, []byte("#EXTM3U")) {
		return true
	}
	if strings.Contains(ct, "video/") || strings.Contains(ct, "octet-stream") || strings.Contains(ct, "mp2t") {
		return len(head) > 0
	}
	return false
}
