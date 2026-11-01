package api

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"tvbox-manager/internal/live"
	"tvbox-manager/internal/model"
	"tvbox-manager/internal/parser"
)

// /tv 直播页的后端支撑：GET /tvapi/live
//
// 直播和点播的取数方式完全不同：点播靠关键词搜索，直播是把整份 m3u/txt 播放列表
// 拉下来展开成频道表。所以这里做三件事：收集平铺源里的 lives 地址 → 并发拉取解析
// → 合并去重后一次性下发。结果缓存 tvLiveTTL，避免每次进页面都去打上游。

const (
	tvLiveTTL        = 10 * time.Minute // 频道表热缓存；命中即秒回
	tvLiveStaleTTL   = 24 * time.Hour  // 落盘缓存作为「降级展示」的有效期：超过热缓存窗口但在此之内，
	                                   // 直接返回旧数据（秒开），后台异步刷新。超过则视为冷启动实时拉取
	tvLiveMaxSources = 50               // 单次聚合的直播源上限，与 parser 提取上限一致
	tvLiveWorkers    = 8                // 并发拉取数：再高容易触发上游限流
	tvLiveDoor       = 12 * time.Second // 单个直播源拉取超时
	tvLiveMaxBody    = 8 << 20          // 播放列表体上限；实测千频道 txt 约 100KB，8MB 极宽松
	tvLiveMaxChans   = 5000             // 频道总数上限，防止异常大文件把响应撑爆
	tvLiveFetchDoor  = 8 * time.Second
)

// tvLiveSourceStat 单个直播源的拉取结果，用于前端提示「哪个源没拉到」
type tvLiveSourceStat struct {
	Name     string `json:"name"`
	URL      string `json:"url"`
	OK       bool   `json:"ok"`
	Channels int    `json:"channels"`
	Error    string `json:"error,omitempty"`
}

// tvLivePayload /tvapi/live 响应体
type tvLivePayload struct {
	Sources  []tvLiveSourceStat `json:"sources"`
	Groups   []string           `json:"groups"`
	Channels []live.Channel     `json:"channels"`
	Count    int                `json:"count"`
	At       time.Time          `json:"at"`
}

var (
	tvLiveMu      sync.Mutex // 守护下面的缓存指针与时间
	tvLiveAt      time.Time
	tvLiveCache   *tvLivePayload
	tvLiveBuildMu sync.Mutex // 串行化「真去上游拉取」的构建：并发请求只一个真拉
	// tvLiveRefreshing 已有后台刷新在排队/进行，防止 invalidate / 预热重复触发。
	tvLiveRefreshing bool
)

var tvLiveClient = &http.Client{
	Timeout:       tvLiveDoor,
	CheckRedirect: tvCheckRedirect,
}

func (a *API) handleTVLive(w http.ResponseWriter, r *http.Request) {
	ok(w, a.tvLive(r.Context()))
}

// tvLive 聚合频道表。三级缓存：内存热缓存(10min) → 落盘降级(24h 秒回+后台刷新) → 冷拉上游。
// 冷启动的并发请求由 tvLiveBuildMu 串行化，只会有一个真去拉上游，其余等结果复用。
func (a *API) tvLive(ctx context.Context) *tvLivePayload {
	// 1) 内存热缓存：命中即秒回
	tvLiveMu.Lock()
	if tvLiveCache != nil && time.Since(tvLiveAt) < tvLiveTTL {
		p := tvLiveCache
		tvLiveMu.Unlock()
		return p
	}
	tvLiveMu.Unlock()

	// 2) 落盘降级：24h 内直接返回旧数据（秒开）；超过热窗口则后台异步刷新
	if a.LiveCacheFile != "" {
		if p, ok := loadLiveCacheFile(a.LiveCacheFile); ok && time.Since(p.At) < tvLiveStaleTTL {
			stale := time.Since(p.At) > tvLiveTTL
			tvLiveMu.Lock()
			tvLiveCache = p
			tvLiveAt = time.Now() // 视为刚填充：10min 内热路径直接命中内存，不再读文件
			tvLiveMu.Unlock()
			if stale {
				a.backgroundRefreshLive()
			}
			return p
		}
	}

	// 3) 冷启动：串行化构建，拉完落盘，下次秒回
	tvLiveBuildMu.Lock()
	defer tvLiveBuildMu.Unlock()
	// 排队期间别人可能已经建好，double-check 直接复用
	tvLiveMu.Lock()
	if tvLiveCache != nil && time.Since(tvLiveAt) < tvLiveTTL {
		p := tvLiveCache
		tvLiveMu.Unlock()
		return p
	}
	tvLiveMu.Unlock()

	p := a.buildLivePayload(ctx)
	tvLiveMu.Lock()
	tvLiveCache = p
	tvLiveAt = time.Now()
	tvLiveMu.Unlock()
	if a.LiveCacheFile != "" {
		saveLiveCacheFile(a.LiveCacheFile, p)
	}
	return p
}

// buildLivePayload 真正去上游拉取并合并排序，返回频道表。耗时操作，调用方负责缓存。
func (a *API) buildLivePayload(ctx context.Context) *tvLivePayload {
	sites := a.tvLiveSites(ctx)
	payload := &tvLivePayload{At: time.Now(), Sources: make([]tvLiveSourceStat, 0, len(sites))}

	type result struct {
		idx      int
		channels []live.Channel
		err      error
	}
	// 预分配结果位，保证输出顺序与源顺序一致（前端据此显示来源）
	results := make([]result, len(sites))
	for i := range results {
		results[i].idx = i
	}
	sem := make(chan struct{}, tvLiveWorkers)
	var wg sync.WaitGroup
	for i, it := range sites {
		wg.Add(1)
		go func(i int, it model.LiveSiteEntry) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			chans, _, err := fetchLivePlaylistCtx(ctx, it.URL, it.Name, tvLiveDoor)
			results[i].channels = chans
			results[i].err = err
		}(i, it)
	}
	wg.Wait()

	var all []live.Channel
	for _, res := range results {
		it := sites[res.idx]
		st := tvLiveSourceStat{Name: it.Name, URL: it.URL, Channels: len(res.channels)}
		if res.err != nil {
			st.Error = res.err.Error()
		} else {
			st.OK = true
			all = append(all, res.channels...)
		}
		payload.Sources = append(payload.Sources, st)
	}

	merged := live.Merge(all)
	// 只保留有线路的频道：没有 urls 的频道点开也播不了，出现在列表/分组/来源筛选里
	// 只会干扰选择。
	merged = filterPlayable(merged)
	if len(merged) > tvLiveMaxChans {
		merged = merged[:tvLiveMaxChans]
	}

	// 分组排序：先按类别权重（央视→卫视→地方→港澳台→其他→平台直播），
	// 同类里频道多的靠前。纯按数量排会把「一起看」「B站直播」顶到最前，
	// 用户真正想看的央视/卫视反而要往下翻。
	counts := make(map[string]int, 32)
	for _, ch := range merged {
		counts[ch.Group]++
	}
	groups := make([]string, 0, len(counts))
	for g := range counts {
		groups = append(groups, g)
	}
	sort.Slice(groups, func(i, j int) bool {
		ri, rj := tvLiveGroupRank(groups[i]), tvLiveGroupRank(groups[j])
		if ri != rj {
			return ri < rj
		}
		if counts[groups[i]] != counts[groups[j]] {
			return counts[groups[i]] > counts[groups[j]]
		}
		return groups[i] < groups[j]
	})

	payload.Channels = merged
	payload.Groups = groups
	payload.Count = len(merged)
	return payload
}

// hasPlayableLine 频道是否有至少一条非空线路。Merge 只保证 URL 非空才追加，
// 但来源数据本身可能只有空串，这里再兜一层，避免把「点开必失败」的频道下发出去。
func hasPlayableLine(ch live.Channel) bool {
	for _, u := range ch.URLs {
		if strings.TrimSpace(u) != "" {
			return true
		}
	}
	return false
}

// filterPlayable 剔除没有可播线路的频道。原地过滤（复用底层数组），不额外分配。
func filterPlayable(chs []live.Channel) []live.Channel {
	if len(chs) == 0 {
		return chs
	}
	kept := chs[:0]
	for _, ch := range chs {
		if hasPlayableLine(ch) {
			kept = append(kept, ch)
		}
	}
	return kept
}

// handleTVLiveRefresh 强制重建频道表（忽略缓存），供直播页「刷新频道」按钮使用。
// 与 GET /tvapi/live 同构返回，前端拿到就能直接重渲染。冷启动/多源并发时耗时
// 可能十几秒，前端按钮侧有 loading 态兜住。
func (a *API) handleTVLiveRefresh(w http.ResponseWriter, r *http.Request) {
	a.refreshLiveCache()
	// 刷新后内存缓存刚写入，tvLive 必然命中热缓存，不会二次打上游
	ok(w, a.tvLive(r.Context()))
}

// refreshLiveCache 强制重建频道表（忽略内存热缓存），实时拉取后更新内存并落盘。
// 由 tvLiveBuildMu 串行化：与冷启动构建冲突时排队复用，不会重复打上游。
func (a *API) refreshLiveCache() {
	tvLiveBuildMu.Lock()
	defer tvLiveBuildMu.Unlock()

	p := a.buildLivePayload(context.Background())
	tvLiveMu.Lock()
	tvLiveCache = p
	tvLiveAt = time.Now()
	tvLiveMu.Unlock()
	if a.LiveCacheFile != "" {
		saveLiveCacheFile(a.LiveCacheFile, p)
	}
}

// backgroundRefreshLive 后台异步刷新频道表（不阻塞当前请求）。
// 同一时刻最多排队一个刷新，避免 invalidate / 预热反复触发把上游打爆。
func (a *API) backgroundRefreshLive() {
	tvLiveMu.Lock()
	if tvLiveRefreshing {
		tvLiveMu.Unlock()
		return
	}
	tvLiveRefreshing = true
	tvLiveMu.Unlock()
	go func() {
		defer func() {
			tvLiveMu.Lock()
			tvLiveRefreshing = false
			tvLiveMu.Unlock()
		}()
		a.refreshLiveCache()
	}()
}

// PreloadLiveChannels 服务启动后调用：后台预热频道表（填充内存+落盘），
// 让用户还没点「直播」时缓存就已经就绪，首次进入秒开。
func (a *API) PreloadLiveChannels() {
	a.backgroundRefreshLive()
}

// tvLiveSites 收集直播源：优先用探活时已入库的 Detail.LiveSites，
// 一条都没有时（旧数据还没重检）现场拉几个单仓补一遍，只读不写库。
func (a *API) tvLiveSites(ctx context.Context) []model.LiveSiteEntry {
	// 首选独立维护的直播源表：管理端可增删改、可停用，是唯一权威来源
	if list := a.st.EnabledLiveSources(); len(list) > 0 {
		out := make([]model.LiveSiteEntry, 0, len(list))
		for _, ls := range list {
			if len(out) >= tvLiveMaxSources {
				break
			}
			out = append(out, model.LiveSiteEntry{Name: ls.Name, URL: ls.URL, Group: ls.Group})
		}
		return out
	}

	// 表为空（刚升级、还没跑过一轮探活同步）→ 退回从各源的 Detail.LiveSites 现取，
	// 保证升级后第一屏就有内容，不用等下次探活。
	seen := make(map[string]bool, 32)
	out := make([]model.LiveSiteEntry, 0, 32)

	add := func(list []model.LiveSiteEntry) {
		for _, it := range list {
			if len(out) >= tvLiveMaxSources {
				return
			}
			if it.URL == "" || seen[it.URL] {
				continue
			}
			seen[it.URL] = true
			out = append(out, it)
		}
	}

	var singles []model.Source
	for _, s := range a.st.AllSources() {
		if !s.Enabled || s.Detail == nil || s.Status != model.StatusOK {
			continue
		}
		add(s.Detail.LiveSites)
		if s.Type == model.TypeSingle && len(singles) < tvFetchLimit {
			singles = append(singles, s)
		}
	}
	if len(out) > 0 {
		return out
	}

	// 兜底：存储层没有直播源，现场解析有限几个单仓
	ck := a.sch.Checker()
	for _, s := range singles {
		if len(out) >= tvLiveMaxSources {
			break
		}
		fctx, cancel := context.WithTimeout(ctx, tvLiveFetchDoor)
		_, body := ck.FetchT(fctx, s.URL, false, int(tvLiveFetchDoor.Seconds()))
		cancel()
		if len(body) == 0 {
			continue
		}
		_, detail, err := parser.Parse(body)
		if err != nil || detail == nil {
			continue
		}
		add(detail.LiveSites)
	}
	return out
}

// invalidateLiveCache 直播源增删改 / 启停 / 检测完成后立即让频道表缓存失效，
// 否则用户改完还得等 10 分钟才看到变化。失效后立刻后台重建（落盘 24h 内的旧数据仍可用，
// 下次进页面秒回旧列表，刷新完成后换成新数据），避免缓存真空期让用户冷拉上游。
func (a *API) invalidateLiveCache() {
	tvLiveMu.Lock()
	tvLiveCache = nil
	tvLiveAt = time.Time{}
	tvLiveMu.Unlock()
	a.backgroundRefreshLive()
}

// loadLiveCacheFile 读取落盘的聚合频道表。失败或为空返回 (nil, false)。
func loadLiveCacheFile(path string) (*tvLivePayload, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var p tvLivePayload
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, false
	}
	if p.At.IsZero() || len(p.Channels) == 0 {
		return nil, false
	}
	return &p, true
}

// saveLiveCacheFile 原子写落盘聚合频道表（临时文件 + rename）。
func saveLiveCacheFile(path string, p *tvLivePayload) {
	b, err := json.Marshal(p)
	if err != nil {
		return
	}
	if dir := filepath.Dir(path); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

type httpStatusError struct{ code int }

func (e *httpStatusError) Error() string { return "上游返回 " + http.StatusText(e.code) }

// tvLiveGroupRank 分组展示权重。平台类（虎牙/斗鱼/B站/YY）频道动辄上千，
// 单看数量一定排最前，所以显式压低到末尾。
func tvLiveGroupRank(name string) int {
	low := strings.ToLower(name)
	has := func(words ...string) bool {
		for _, w := range words {
			if strings.Contains(low, strings.ToLower(w)) {
				return true
			}
		}
		return false
	}
	switch {
	case has("央视", "cctv", "中央"):
		return 0
	case has("卫视"):
		return 1
	case has("地方", "城市", "省内", "频道"):
		return 2
	case has("港澳", "香港", "台湾", "澳门", "海外"):
		return 3
	case has("一起看", "轮播", "直播", "虎牙", "斗鱼", "b站", "yy", "影视", "记录", "音乐", "电影", "游戏"):
		return 8
	}
	return 5
}

var errBadLiveURL = &liveURLError{}

type liveURLError struct{}

func (e *liveURLError) Error() string { return "不是有效的 http/https 地址" }
