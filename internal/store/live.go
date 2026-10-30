package store

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"tvbox-manager/internal/model"
	"tvbox-manager/internal/parser"
)

// 直播源存储层。
//
// 与站点源（flat.go）的设计对齐：auto 源由父源探活结果同步生成，manual 源独立存在；
// 删除 auto 源留墓碑（Hidden），父源再次同步也不复活。区别在于直播源多了一组
// 「解析结果」指标（频道数/分组数/抽样可用数）——一份播放列表展开后就是几百上千个频道，
// 光看「能不能拉下来」说明不了问题。

// LiveFilter 直播源列表过滤条件
type LiveFilter struct {
	Keyword string // 名称/地址/备注
	Status  string // ok / fail / checking / unknown
	Group   string
	Enabled string // "true" / "false"
	Origin  string // auto / manual
	Page    int
	Size    int
	SortBy  string // name / status / url / group / channels / latency / time / enabled
	SortDir string // asc / desc
}

// LiveCheckResult 单次检测结果
type LiveCheckResult struct {
	Status       string
	HTTPCode     int
	LatencyMs    int64
	Msg          string
	ChannelCount int
	GroupCount   int
	AliveCount   int
	SampleCount  int
}

// NewLiveID 生成直播源 ID
func NewLiveID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return "live_" + hex.EncodeToString(b)
}

// liveKey 去重键：剥掉 | 请求头参数后按 DedupKey 归一化。
// 同一地址带不同 UA 是两条记录就没意义了，拉下来是同一份频道表。
func liveKey(raw string) string {
	stem := raw
	if i := strings.Index(raw, "|"); i >= 0 {
		stem = raw[:i]
	}
	return parser.DedupKey(strings.TrimSpace(stem))
}

// splitLiveHeader 与 live.SplitHeader 同义；store 不依赖 live 包，避免包间耦合。
func liveURLStem(raw string) string {
	if i := strings.Index(raw, "|"); i >= 0 {
		return strings.TrimSpace(raw[:i])
	}
	return strings.TrimSpace(raw)
}

func defaultLiveGroup(g string) string {
	g = strings.TrimSpace(g)
	if g == "" {
		return "默认"
	}
	return g
}

func cloneLive(l model.LiveSource) model.LiveSource {
	c := l
	if l.ParentIDs != nil {
		c.ParentIDs = append([]string(nil), l.ParentIDs...)
	}
	if l.ParentNames != nil {
		c.ParentNames = append([]string(nil), l.ParentNames...)
	}
	return c
}

// ---------- 查询 ----------

// ListLiveSourcesPage 返回当前页与总数（一次快照，避免列表/计数跑两遍）
func (s *Store) ListLiveSourcesPage(f LiveFilter) ([]model.LiveSource, int) {
	s.mu.RLock()
	out := make([]model.LiveSource, 0, len(s.data.LiveSources))
	nameOf := make(map[string]string, len(s.data.Sources))
	for i := range s.data.Sources {
		nameOf[s.data.Sources[i].ID] = s.data.Sources[i].Name
	}
	for i := range s.data.LiveSources {
		if s.data.LiveSources[i].Hidden {
			continue
		}
		ls := cloneLive(s.data.LiveSources[i])
		ls.ParentNames = nil
		for _, pid := range ls.ParentIDs {
			if n, ok := nameOf[pid]; ok {
				ls.ParentNames = append(ls.ParentNames, n)
			}
		}
		out = append(out, ls)
	}
	s.mu.RUnlock()

	out = filterLive(out, f)
	total := len(out)
	sortLive(out, f)
	return pageLive(out, f), total
}

func (s *Store) AllLiveSources() []model.LiveSource {
	list, _ := s.ListLiveSourcesPage(LiveFilter{})
	return list
}

// EnabledLiveSources /tvapi/live 的数据源：启用、未删除、且不是明确检测失败的。
// 失败的源拉下来也是空频道表，直接跳过能省掉一轮无谓的上游请求。
func (s *Store) EnabledLiveSources() []model.LiveSource {
	s.mu.RLock()
	out := make([]model.LiveSource, 0, len(s.data.LiveSources))
	for i := range s.data.LiveSources {
		ls := s.data.LiveSources[i]
		if ls.Hidden || !ls.Enabled || ls.Status == model.StatusFail {
			continue
		}
		out = append(out, cloneLive(ls))
	}
	s.mu.RUnlock()
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Sort != out[j].Sort {
			return out[i].Sort < out[j].Sort
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// GetLiveSource 单条副本（含已删除，供内部按 ID 操作）
func (s *Store) GetLiveSource(id string) (model.LiveSource, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for i := range s.data.LiveSources {
		if s.data.LiveSources[i].ID == id {
			return cloneLive(s.data.LiveSources[i]), true
		}
	}
	return model.LiveSource{}, false
}

// LiveGroups 直播源分组（不含已删除的）
func (s *Store) LiveGroups() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	seen := map[string]bool{}
	for i := range s.data.LiveSources {
		if s.data.LiveSources[i].Hidden {
			continue
		}
		seen[defaultLiveGroup(s.data.LiveSources[i].Group)] = true
	}
	out := make([]string, 0, len(seen))
	for g := range seen {
		out = append(out, g)
	}
	sort.Strings(out)
	return out
}

// LiveStats 概览用：总数/启用/可用/失效/检测中
func (s *Store) LiveStats() map[string]int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	stat := map[string]int{"total": 0, "enabled": 0, "ok": 0, "fail": 0, "checking": 0, "unknown": 0}
	for i := range s.data.LiveSources {
		ls := s.data.LiveSources[i]
		if ls.Hidden {
			continue
		}
		stat["total"]++
		if ls.Enabled {
			stat["enabled"]++
		}
		switch ls.Status {
		case model.StatusOK:
			stat["ok"]++
		case model.StatusFail:
			stat["fail"]++
		case model.StatusChecking:
			stat["checking"]++
		default:
			stat["unknown"]++
		}
	}
	return stat
}

func filterLive(in []model.LiveSource, f LiveFilter) []model.LiveSource {
	out := in[:0:0]
	kw := strings.ToLower(strings.TrimSpace(f.Keyword))
	for _, x := range in {
		if kw != "" {
			hit := strings.Contains(strings.ToLower(x.Name), kw) ||
				strings.Contains(strings.ToLower(x.URL), kw) ||
				strings.Contains(strings.ToLower(x.Remark), kw)
			if !hit {
				continue
			}
		}
		if f.Status != "" && f.Status != "all" && x.Status != f.Status {
			continue
		}
		if f.Group != "" && f.Group != "all" && defaultLiveGroup(x.Group) != f.Group {
			continue
		}
		if f.Enabled == "true" || f.Enabled == "false" {
			if (f.Enabled == "true") != x.Enabled {
				continue
			}
		}
		if f.Origin != "" && f.Origin != "all" && x.Origin != f.Origin {
			continue
		}
		out = append(out, x)
	}
	return out
}

func sortLive(out []model.LiveSource, f LiveFilter) {
	key := f.SortBy
	if key == "" {
		key = "default"
	}
	desc := f.SortDir == "desc"
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		var cmp int
		switch key {
		case "name":
			cmp = strings.Compare(a.Name, b.Name)
		case "status":
			cmp = liveStatusRank(a.Status) - liveStatusRank(b.Status)
		case "url":
			cmp = strings.Compare(a.URL, b.URL)
		case "group":
			cmp = strings.Compare(defaultLiveGroup(a.Group), defaultLiveGroup(b.Group))
		case "channels":
			cmp = a.ChannelCount - b.ChannelCount
		case "latency":
			cmp = int(a.LatencyMs - b.LatencyMs)
		case "time":
			cmp = cmpTime(a.LastCheckAt, b.LastCheckAt)
		case "enabled":
			cmp = boolRank(a.Enabled) - boolRank(b.Enabled)
		default:
			// 默认：启用优先，再按 sort/名称，手动源排在自动源前面（自己加的更容易被找）
			if a.Enabled != b.Enabled {
				return a.Enabled
			}
			if a.Origin != b.Origin {
				return a.Origin == model.LiveOriginManual
			}
			if a.Sort != b.Sort {
				return a.Sort < b.Sort
			}
			return a.Name < b.Name
		}
		if cmp == 0 {
			return a.Name < b.Name
		}
		if desc {
			return cmp > 0
		}
		return cmp < 0
	})
}

func pageLive(out []model.LiveSource, f LiveFilter) []model.LiveSource {
	if f.Page > 0 && f.Size > 0 {
		start := (f.Page - 1) * f.Size
		if start >= len(out) {
			return []model.LiveSource{}
		}
		end := start + f.Size
		if end > len(out) {
			end = len(out)
		}
		out = out[start:end]
	}
	return out
}

func liveStatusRank(s string) int {
	switch s {
	case model.StatusOK:
		return 0
	case model.StatusChecking:
		return 1
	case model.StatusFail:
		return 2
	}
	return 3
}

func boolRank(b bool) int {
	if b {
		return 0
	}
	return 1
}

func cmpTime(a, b *time.Time) int {
	if a == nil && b == nil {
		return 0
	}
	if a == nil {
		return -1
	}
	if b == nil {
		return 1
	}
	return int(a.Sub(*b))
}

// ---------- 新增 ----------

// AddLiveSource 手动添加一条（URL 已归一去重由调用方保证，这里兜底再查一次）
func (s *Store) AddLiveSource(name, rawURL, group, remark string, enabled bool, timeoutSeconds int) (model.LiveSource, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return model.LiveSource{}, fmt.Errorf("地址为空")
	}
	key := liveKey(rawURL)

	s.mu.Lock()
	defer s.mu.Unlock()
	if idx := s.findLiveByKeyLocked(key); idx >= 0 {
		ls := s.data.LiveSources[idx]
		if ls.Hidden {
			// 之前删过同地址：把手动添加当作「恢复」，清掉墓碑
			ls.Hidden = false
			ls.Enabled = enabled
			ls.Origin = model.LiveOriginManual
			ls.Name = strings.TrimSpace(name)
			if ls.Name == "" {
				ls.Name = liveURLStem(rawURL)
			}
			ls.Group = defaultLiveGroup(group)
			ls.Remark = strings.TrimSpace(remark)
			ls.NameAuto = false
			ls.GroupAuto = false
			ls.Status = model.StatusUnknown
			ls.UpdatedAt = time.Now()
			s.data.LiveSources[idx] = ls
			_ = s.flush()
			return cloneLive(ls), nil
		}
		return model.LiveSource{}, fmt.Errorf("地址已存在：%s", ls.Name)
	}

	now := time.Now()
	ls := model.LiveSource{
		ID:             NewLiveID(),
		Name:           strings.TrimSpace(name),
		URL:            rawURL,
		Group:          defaultLiveGroup(group),
		Remark:         strings.TrimSpace(remark),
		Enabled:        enabled,
		Origin:         model.LiveOriginManual,
		TimeoutSeconds: timeoutSeconds,
		Status:         model.StatusUnknown,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if ls.Name == "" {
		ls.Name = liveURLStem(rawURL)
	}
	s.data.LiveSources = append(s.data.LiveSources, ls)
	if err := s.flush(); err != nil {
		return model.LiveSource{}, err
	}
	return cloneLive(ls), nil
}

// AddLiveSources 批量添加（多行粘贴）。返回新增条目的 ID 与跳过原因。
// 返回 ID 是为了让调用方能立刻对这些新条目发起检测——只给数量的话，
// 批量添加完还得回查一遍才知道该测谁。
func (s *Store) AddLiveSources(items []model.LiveSource) ([]string, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, len(items))
	var skipped []string
	for _, it := range items {
		key := liveKey(it.URL)
		if key == "" {
			continue
		}
		if idx := s.findLiveByKeyLocked(key); idx >= 0 {
			ex := s.data.LiveSources[idx]
			skipped = append(skipped, fmt.Sprintf("%s（%s）", it.Name, "地址已存在："+ex.Name))
			continue
		}
		now := time.Now()
		ls := it
		ls.ID = NewLiveID()
		ls.Name = strings.TrimSpace(ls.Name)
		if ls.Name == "" {
			ls.Name = liveURLStem(ls.URL)
		}
		ls.Group = defaultLiveGroup(ls.Group)
		ls.Origin = model.LiveOriginManual
		ls.NameAuto = false
		ls.GroupAuto = false
		if ls.Status == "" {
			ls.Status = model.StatusUnknown
		}
		ls.CreatedAt = now
		ls.UpdatedAt = now
		s.data.LiveSources = append(s.data.LiveSources, ls)
		ids = append(ids, ls.ID)
	}
	if len(ids) > 0 {
		_ = s.flush()
	}
	return ids, skipped
}

func (s *Store) findLiveByKeyLocked(key string) int {
	if key == "" {
		return -1
	}
	for i := range s.data.LiveSources {
		if liveKey(s.data.LiveSources[i].URL) == key {
			return i
		}
	}
	return -1
}

// ---------- 修改 ----------

// UpdateLiveSource 修改直播源（手动改名/改组会关闭对应字段的自动同步）
func (s *Store) UpdateLiveSource(id string, fn func(*model.LiveSource)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.data.LiveSources {
		if s.data.LiveSources[i].ID != id {
			continue
		}
		up := cloneLive(s.data.LiveSources[i])
		fn(&up)
		up.UpdatedAt = time.Now()
		s.data.LiveSources[i] = up
		return s.flush()
	}
	return fmt.Errorf("直播源不存在: %s", id)
}

// SetLiveChecking 标记检测中（只改内存，检测结束后一起落盘）
func (s *Store) SetLiveChecking(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.data.LiveSources {
		if s.data.LiveSources[i].ID == id {
			s.data.LiveSources[i].Status = model.StatusChecking
			return
		}
	}
}

// UpdateLiveCheckResult 回写检测结果
func (s *Store) UpdateLiveCheckResult(id string, r LiveCheckResult, persist bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.data.LiveSources {
		if s.data.LiveSources[i].ID != id {
			continue
		}
		ls := s.data.LiveSources[i]
		now := time.Now()
		ls.LastCheckAt = &now
		ls.Status = r.Status
		ls.HTTPCode = r.HTTPCode
		ls.LatencyMs = r.LatencyMs
		ls.CheckMsg = r.Msg
		ls.CheckCount++
		ls.ChannelCount = r.ChannelCount
		ls.GroupCount = r.GroupCount
		ls.AliveCount = r.AliveCount
		ls.SampleCount = r.SampleCount
		if r.Status == model.StatusOK {
			ls.OKStreak++
			ls.FailStreak = 0
			ls.LastOKAt = &now
		} else {
			ls.FailStreak++
			ls.OKStreak = 0
			ls.FailCount++
			// 检测失败不代表地址永久不可用（直播源经常临时抽风），
			// 这里不做自动禁用：禁用了用户会以为源被删了。
		}
		s.data.LiveSources[i] = ls
		if persist {
			return s.flush()
		}
		return nil
	}
	return fmt.Errorf("直播源不存在: %s", id)
}

// ---------- 删除 / 批量 ----------

// DeleteLiveSources 删除。auto 源留墓碑（父源再同步不复活），manual 源直接移除。
func (s *Store) DeleteLiveSources(ids []string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	kept := s.data.LiveSources[:0]
	n := 0
	for _, x := range s.data.LiveSources {
		if want[x.ID] {
			n++
			if x.Origin == model.LiveOriginAuto {
				x.Hidden = true
				x.Enabled = false
				x.UpdatedAt = time.Now()
				kept = append(kept, x)
			}
			continue
		}
		kept = append(kept, x)
	}
	s.data.LiveSources = kept
	if n > 0 {
		_ = s.flush()
	}
	return n
}

// BatchLiveSources 批量 enable / disable / delete / group
func (s *Store) BatchLiveSources(ids []string, action, group string) (int, error) {
	if action == "delete" {
		return s.DeleteLiveSources(ids), nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	n := 0
	for i := range s.data.LiveSources {
		if !want[s.data.LiveSources[i].ID] {
			continue
		}
		switch action {
		case "enable":
			s.data.LiveSources[i].Enabled = true
		case "disable":
			s.data.LiveSources[i].Enabled = false
		case "group":
			s.data.LiveSources[i].Group = defaultLiveGroup(group)
			s.data.LiveSources[i].GroupAuto = false
		default:
			return 0, fmt.Errorf("未知操作: %s", action)
		}
		s.data.LiveSources[i].UpdatedAt = time.Now()
		n++
	}
	if n > 0 {
		return n, s.flush()
	}
	return 0, nil
}

// ---------- 与平铺源同步 ----------

// LiveSyncStat 一次 lives 对账的统计，用于「一键解析」给用户交代结果。
type LiveSyncStat struct {
	Added    int `json:"added"`    // 新增的直播源
	Linked   int `json:"linked"`   // 已存在、本次补上了父源关联
	Detached int `json:"detached"` // 已存在、但父源里已没有该地址，解除关联
	Existed  int `json:"existed"`  // 已存在且无变化（地址判重命中，直接忽略）
	Total    int `json:"total"`    // 本次解析出的直播源地址数
}

// SyncLiveSourcesForSource 父源探活成功后，把 Detail.LiveSites 对账进直播源表。
// 调用方需保证 src 已是最新值（updateCheckResultLocked 内部调用即满足）。
func (s *Store) SyncLiveSourcesForSource(src model.Source) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, changed := s.syncLiveSourcesLocked(src); changed {
		_ = s.flush()
	}
}

// SyncLiveSites 用「重新解析」得到的 lives[] 覆盖源详情并同步进直播源表。
// 与探活解耦：只关心直播地址，不重跑站点解析。
//
// 详情只在原本就非空时才写回——从未探活成功的源（Detail == nil）如果被塞一个
// 只含 LiveSites 的详情，订阅平铺那边会把它当成多仓/单仓配置读，语义就错了。
// 这种情况下只同步直播源表，详情留给下一次探活。
func (s *Store) SyncLiveSites(sourceID string, sites []model.LiveSiteEntry) (LiveSyncStat, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	idx := -1
	for i := range s.data.Sources {
		if s.data.Sources[i].ID == sourceID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return LiveSyncStat{Total: len(sites)}, false
	}

	detailUpdated := false
	if s.data.Sources[idx].Detail != nil {
		sp := clone(s.data.Sources[idx])
		d := *sp.Detail
		d.LiveSites = sites
		sp.Detail = &d
		sp.UpdatedAt = time.Now()
		s.data.Sources[idx] = sp
		detailUpdated = true
	}

	// 对账用副本：Detail 为空时也要按本次解析结果挂父源关联
	sp := clone(s.data.Sources[idx])
	sp.Detail = &model.Detail{LiveSites: sites}
	stat, changed := s.syncLiveSourcesLocked(sp)
	if changed || detailUpdated {
		_ = s.flush()
	}
	return stat, detailUpdated
}

func (s *Store) syncLiveSourcesLocked(src model.Source) (LiveSyncStat, bool) {
	stat := LiveSyncStat{}
	changed := false
	want := make(map[string]model.LiveSiteEntry, 8)
	if src.Detail != nil {
		for _, e := range src.Detail.LiveSites {
			if e.URL == "" {
				continue
			}
			want[liveKey(e.URL)] = e
		}
	}

	// 1) 新增 / 更新
	stat.Total = len(want)
	for key, e := range want {
		idx := s.findLiveByKeyLocked(key)
		if idx < 0 {
			now := time.Now()
			name := strings.TrimSpace(e.Name)
			if name == "" {
				name = liveURLStem(e.URL)
			}
			s.data.LiveSources = append(s.data.LiveSources, model.LiveSource{
				ID:        NewLiveID(),
				Name:      name,
				URL:       e.URL,
				Group:     defaultLiveGroup(e.Group),
				Enabled:   true,
				Origin:    model.LiveOriginAuto,
				ParentIDs: []string{src.ID},
				NameAuto:  true,
				GroupAuto: true,
				Sort:      src.Sort,
				Status:    model.StatusUnknown,
				CreatedAt: now,
				UpdatedAt: now,
			})
			changed = true
			stat.Added++
			continue
		}

		ls := s.data.LiveSources[idx]
		if ls.Hidden {
			stat.Existed++ // 用户删过，不复活，也不重复添加
			continue
		}
		dirty := false
		if !containsStr(ls.ParentIDs, src.ID) {
			ls.ParentIDs = append(ls.ParentIDs, src.ID)
			dirty = true
			stat.Linked++
		}
		// manual 源被父源也引用到时，保留用户起的名字
		if ls.Origin == model.LiveOriginAuto {
			if ls.NameAuto {
				if n := strings.TrimSpace(e.Name); n != "" && n != ls.Name {
					ls.Name = n
					dirty = true
				}
			}
			if ls.GroupAuto {
				if g := defaultLiveGroup(e.Group); g != ls.Group && strings.TrimSpace(e.Group) != "" {
					ls.Group = g
					dirty = true
				}
			}
		}
		if dirty {
			ls.UpdatedAt = time.Now()
			s.data.LiveSources[idx] = ls
			changed = true
		} else {
			stat.Existed++ // 地址判重命中：已在表里、无需任何变更
		}
	}

	// 2) 父源里已消失的地址：解除关联。不删记录——可能是用户正在用的手动命名条目，
	//    留成「孤儿」由用户在页面上决定清理，比自动删除安全。
	//    墓碑（已删除）也一并解除，保证隐藏条目的父引用是准的，将来恢复时不会挂错父源。
	for i := range s.data.LiveSources {
		ls := &s.data.LiveSources[i]
		if ls.Origin != model.LiveOriginAuto {
			continue
		}
		if !containsStr(ls.ParentIDs, src.ID) {
			continue
		}
		if _, ok := want[liveKey(ls.URL)]; ok {
			continue
		}
		ls.ParentIDs = removeStr(ls.ParentIDs, src.ID)
		ls.UpdatedAt = time.Now()
		changed = true
		stat.Detached++
	}
	return stat, changed
}

// DetachLiveSourceParent 父源被删除/地址变更时调用：从所有 auto 源解除关联。
func (s *Store) DetachLiveSourceParent(sourceID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	for i := range s.data.LiveSources {
		ls := &s.data.LiveSources[i]
		if !containsStr(ls.ParentIDs, sourceID) {
			continue
		}
		ls.ParentIDs = removeStr(ls.ParentIDs, sourceID)
		ls.UpdatedAt = time.Now()
		changed = true
	}
	if changed {
		_ = s.flush()
	}
}

func containsStr(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func removeStr(list []string, v string) []string {
	out := list[:0]
	for _, x := range list {
		if x != v {
			out = append(out, x)
		}
	}
	return out
}
