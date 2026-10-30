package api

import (
	"context"
	"net/http"
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
	tvLiveTTL        = 10 * time.Minute // 频道表缓存；直播源变动不频繁，10 分钟足够
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
	tvLiveMu    sync.Mutex
	tvLiveAt    time.Time
	tvLiveCache *tvLivePayload
)

var tvLiveClient = &http.Client{
	Timeout:       tvLiveDoor,
	CheckRedirect: tvCheckRedirect,
}

func (a *API) handleTVLive(w http.ResponseWriter, r *http.Request) {
	ok(w, a.tvLive(r.Context()))
}

// tvLive 聚合频道表。串行化整段拉取：冷启动时多个并发请求只会有一个真去拉上游，
// 其余在锁上等结果，不会把 50 个源并发翻好几倍。
func (a *API) tvLive(ctx context.Context) *tvLivePayload {
	tvLiveMu.Lock()
	defer tvLiveMu.Unlock()
	if tvLiveCache != nil && time.Since(tvLiveAt) < tvLiveTTL {
		return tvLiveCache
	}

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
	tvLiveCache = payload
	tvLiveAt = time.Now()
	return payload
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
// 否则用户改完还得等 10 分钟才看到变化。
func (a *API) invalidateLiveCache() {
	tvLiveMu.Lock()
	tvLiveCache = nil
	tvLiveAt = time.Time{}
	tvLiveMu.Unlock()
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
