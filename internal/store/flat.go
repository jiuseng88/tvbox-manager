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

// FlatFilter 站点源列表过滤条件，全部字段可选
type FlatFilter struct {
	Keyword string // 名称/URL/备注 模糊匹配
	Status  string // ok / fail / unknown
	Group   string
	Enabled string // "true"/"false"，按有效启用（自身 && 任一父源启用）过滤
	Page    int
	Size    int
	Sort    string // 默认 sort+name；"failFirst" 失效置顶
	SortBy  string // 表头排序键（优先于 Sort）
	SortDir string // 表头排序方向：asc / desc
}

// ---------- 查询 ----------

func cloneFlat(f model.FlatSource) model.FlatSource {
	c := f
	c.ParentIDs = append([]string(nil), f.ParentIDs...)
	return c
}

// effectiveEnabled 自身启用 && 未隐藏 && 任一父源启用
func effectiveEnabled(f model.FlatSource, idx map[string]model.Source) bool {
	if !f.Enabled || f.Hidden || len(f.ParentIDs) == 0 {
		return false
	}
	for _, pid := range f.ParentIDs {
		if p, ok := idx[pid]; ok && p.Enabled {
			return true
		}
	}
	return false
}

// parentNames 父源名称列表（父源已删除的引用忽略）
func parentNames(f model.FlatSource, idx map[string]model.Source) []string {
	names := make([]string, 0, len(f.ParentIDs))
	for _, pid := range f.ParentIDs {
		if p, ok := idx[pid]; ok {
			names = append(names, p.Name)
		}
	}
	return names
}

// srcIndexLocked 父源 ID → 父源 索引；避免每条站点源都线性扫一遍 sources
func (s *Store) srcIndexLocked() map[string]model.Source {
	idx := make(map[string]model.Source, len(s.data.Sources))
	for i := range s.data.Sources {
		idx[s.data.Sources[i].ID] = s.data.Sources[i]
	}
	return idx
}

// computeFlatLocked 回填运行时字段（有效启用、父源名称）
func (s *Store) computeFlatLocked(f *model.FlatSource, idx map[string]model.Source) {
	f.EffectiveEnabled = effectiveEnabled(*f, idx)
	f.ParentNames = parentNames(*f, idx)
}

// flatsViewLocked 克隆站点源并回填运行时字段，跳过已隐藏项
func (s *Store) flatsViewLocked() ([]model.FlatSource, map[string]model.Source) {
	idx := s.srcIndexLocked()
	out := make([]model.FlatSource, 0, len(s.data.Flats))
	for i := range s.data.Flats {
		if s.data.Flats[i].Hidden {
			continue
		}
		fc := cloneFlat(s.data.Flats[i])
		fc.EffectiveEnabled = effectiveEnabled(fc, idx)
		fc.ParentNames = parentNames(fc, idx)
		out = append(out, fc)
	}
	return out, idx
}

// applyFlatsFilter 就地过滤
func applyFlatsFilter(out []model.FlatSource, f FlatFilter) []model.FlatSource {
	if f.Keyword != "" {
		kw := strings.ToLower(f.Keyword)
		out = filterFlats(out, func(x model.FlatSource) bool {
			return strings.Contains(strings.ToLower(x.Name), kw) ||
				strings.Contains(strings.ToLower(x.URL), kw) ||
				strings.Contains(strings.ToLower(x.Remark), kw)
		})
	}
	if f.Status != "" && f.Status != "all" {
		out = filterFlats(out, func(x model.FlatSource) bool { return x.Status == f.Status })
	}
	if f.Group != "" && f.Group != "all" {
		out = filterFlats(out, func(x model.FlatSource) bool { return x.Group == f.Group })
	}
	if f.Enabled == "true" || f.Enabled == "false" {
		out = filterFlats(out, func(x model.FlatSource) bool {
			return (f.Enabled == "true") == x.EffectiveEnabled
		})
	}
	return out
}

func sortFlats(out []model.FlatSource, f FlatFilter) {
	if f.SortBy != "" {
		// 表头排序优先
		sortByHeader(out, func(a, b model.FlatSource) int { return cmpFlat(a, b, f.SortBy) }, f.SortDir == "desc", flatFallback)
		return
	}
	sort.SliceStable(out, func(i, j int) bool {
		if f.Sort == "failFirst" {
			fi, fj := statusRank(out[i].Status), statusRank(out[j].Status)
			if fi != fj {
				return fi < fj
			}
		}
		if out[i].Sort != out[j].Sort {
			return out[i].Sort < out[j].Sort
		}
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].URL < out[j].URL // 兜底定序：保证跨请求顺序稳定，翻页不跳动
	})
}

func pageFlats(out []model.FlatSource, f FlatFilter) []model.FlatSource {
	if f.Page > 0 && f.Size > 0 {
		start := (f.Page - 1) * f.Size
		if start >= len(out) {
			return []model.FlatSource{}
		}
		end := start + f.Size
		if end > len(out) {
			end = len(out)
		}
		out = out[start:end]
	}
	return out
}

// ListFlats 过滤 + 排序后返回副本
func (s *Store) ListFlats(f FlatFilter) []model.FlatSource {
	out, total := s.ListFlatsPage(f)
	_ = total
	return out
}

// ListFlatsPage 一次快照同时得到当前页与总数，避免列表/计数各跑一遍全量
func (s *Store) ListFlatsPage(f FlatFilter) ([]model.FlatSource, int) {
	s.mu.RLock()
	out, _ := s.flatsViewLocked()
	s.mu.RUnlock()

	out = applyFlatsFilter(out, f)
	total := len(out)
	sortFlats(out, f)
	return pageFlats(out, f), total
}

func filterFlats(in []model.FlatSource, keep func(model.FlatSource) bool) []model.FlatSource {
	out := in[:0:0]
	for _, x := range in {
		if keep(x) {
			out = append(out, x)
		}
	}
	return out
}

// CountFlats 按过滤条件计数
func (s *Store) CountFlats(f FlatFilter) int {
	f.Page, f.Size = 0, 0
	return len(s.ListFlats(f))
}

// GetFlat 单条副本（含已隐藏项，供内部按 ID 操作）
func (s *Store) GetFlat(id string) (model.FlatSource, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for i := range s.data.Flats {
		if s.data.Flats[i].ID == id {
			fc := cloneFlat(s.data.Flats[i])
			s.computeFlatLocked(&fc, s.srcIndexLocked())
			return fc, true
		}
	}
	return model.FlatSource{}, false
}

// EnabledFlats 订阅输出用：有效启用的站点源，按 sort、name 排序
func (s *Store) EnabledFlats() []model.FlatSource {
	return s.ListFlats(FlatFilter{Enabled: "true"})
}

// FlatGroups 所有站点源分组名（去重排序）
func (s *Store) FlatGroups() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	seen := map[string]bool{}
	for i := range s.data.Flats {
		g := s.data.Flats[i].Group
		if g == "" {
			g = "默认"
		}
		seen[g] = true
	}
	out := make([]string, 0, len(seen))
	for g := range seen {
		out = append(out, g)
	}
	sort.Strings(out)
	return out
}

// ---------- 修改 ----------

func NewFlatID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return "flat_" + hex.EncodeToString(b)
}

// UpdateFlat 修改站点源
func (s *Store) UpdateFlat(id string, fn func(*model.FlatSource)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.data.Flats {
		if s.data.Flats[i].ID == id {
			updated := cloneFlat(s.data.Flats[i])
			fn(&updated)
			updated.UpdatedAt = time.Now()
			s.computeFlatLocked(&updated, s.srcIndexLocked())
			s.data.Flats[i] = updated
			return s.flush()
		}
	}
	return fmt.Errorf("站点源不存在: %s", id)
}

// UpdateFlatInMemory 只改内存不落盘（置 checking 状态用）
func (s *Store) UpdateFlatInMemory(id string, fn func(*model.FlatSource)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx := s.srcIndexLocked()
	for i := range s.data.Flats {
		if s.data.Flats[i].ID == id {
			updated := cloneFlat(s.data.Flats[i])
			fn(&updated)
			s.computeFlatLocked(&updated, idx)
			s.data.Flats[i] = updated
			return
		}
	}
}

// UpdateFlatCheckResult 站点源探活结果回写
func (s *Store) UpdateFlatCheckResult(id string, r CheckResult, persist bool) error {
	apply := func(fp *model.FlatSource) {
		now := time.Now()
		fp.LastCheckAt = &now
		fp.Status = r.Status
		fp.HTTPCode = r.HTTPCode
		fp.LatencyMs = r.LatencyMs
		fp.CheckMsg = r.Msg
		fp.CheckCount++
		if r.Status == model.StatusOK {
			fp.LastOKAt = &now
		} else {
			fp.FailCount++
		}
	}
	if persist {
		return s.UpdateFlat(id, apply)
	}
	s.UpdateFlatInMemory(id, apply)
	return nil
}

// DeleteFlats 删除站点源并立即落盘。
//
// 删除是「隐藏 + 禁用」而非物理删除：站点源由父源同步生成，若物理删除，
// 下次父源同步会按同一 URL 重新生成，用户删掉的条目就会复活且 ID 变化。
// 置 Hidden 后同步逻辑会跳过它，列表/计数/订阅也一律不再输出。
func (s *Store) DeleteFlats(ids []string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.deleteFlatsLocked(ids)
	if n == 0 {
		return 0, nil
	}
	return n, s.flush()
}

// deleteFlatsLocked 就地置隐藏标记，返回本次由可见变为隐藏的条数。
// 调用方需持有写锁并负责落盘。
func (s *Store) deleteFlatsLocked(ids []string) int {
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	n := 0
	now := time.Now()
	for i := range s.data.Flats {
		f := &s.data.Flats[i]
		if !want[f.ID] || f.Hidden {
			continue
		}
		f.Hidden = true
		f.Enabled = false
		f.EffectiveEnabled = false
		f.UpdatedAt = now
		n++
	}
	return n
}

// BatchFlats 站点源批量启用/禁用/改组/删除
func (s *Store) BatchFlats(ids []string, action, group string) (int, error) {
	switch action {
	case "enable", "disable":
		want := (action == "enable")
		n := 0
		for _, id := range ids {
			if err := s.UpdateFlat(id, func(fp *model.FlatSource) { fp.Enabled = want }); err == nil {
				n++
			}
		}
		return n, nil
	case "delete":
		n, err := s.DeleteFlats(ids)
		return n, err
	case "group":
		if group == "" {
			return 0, fmt.Errorf("改组需要 group 参数")
		}
		n := 0
		for _, id := range ids {
			// 手动批量改组后不再随父源同步分组
			if err := s.UpdateFlat(id, func(fp *model.FlatSource) {
				fp.Group = group
				fp.GroupAuto = false
			}); err == nil {
				n++
			}
		}
		return n, nil
	default:
		return 0, fmt.Errorf("未知批量操作: %s", action)
	}
}

// ---------- 与父源的同步 ----------

func containsID(ids []string, id string) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

func removeID(ids []string, id string) []string {
	out := ids[:0]
	for _, x := range ids {
		if x != id {
			out = append(out, x)
		}
	}
	return out
}

// upsertFlatLocked 按归一化 URL 创建/更新站点源。已存在时补父引用；
// forceName=true（单仓父源）时名称随父源同步，否则首次出现的名称优先（不覆盖）；
// GroupAuto 时同步父源分组。返回是否产生了实际变化（用于决定要不要落盘）。
//
// 已隐藏（用户删除过）的 URL 直接跳过：父源再次同步也不复活，
// 同时不改动它的任何字段，保证用户意图不被覆盖。
func (s *Store) upsertFlatLocked(parent model.Source, rawURL, name string, forceName bool) bool {
	norm, err := parser.Normalize(rawURL)
	if err != nil {
		return false
	}
	group := parent.Group
	if group == "" {
		group = "默认"
	}
	for i := range s.data.Flats {
		f := &s.data.Flats[i]
		if f.URL != norm {
			continue
		}
		if f.Hidden {
			return false
		}
		changed := false
		if !containsID(f.ParentIDs, parent.ID) {
			f.ParentIDs = append(f.ParentIDs, parent.ID)
			changed = true
		}
		if name != "" && f.Name != name && (f.Name == "" || (forceName && f.NameAuto)) {
			f.Name = name
			changed = true
		}
		if f.GroupAuto && f.Group != group {
			f.Group = group
			changed = true
		}
		if changed {
			f.UpdatedAt = time.Now()
		}
		return changed
	}
	now := time.Now()
	if name == "" {
		name = parent.Name
	}
	s.data.Flats = append(s.data.Flats, model.FlatSource{
		ID:        NewFlatID(),
		Name:      name,
		URL:       norm,
		ParentIDs: []string{parent.ID},
		Enabled:   true,
		Group:     group,
		Sort:      parent.Sort,
		NameAuto:  true,
		GroupAuto: true,
		Status:    model.StatusUnknown,
		CreatedAt: now,
		UpdatedAt: now,
	})
	return true
}

// syncFlatsForSourceLocked 父源变化后对齐其关联站点源：
//   - 单仓/未知：自身地址生成一条站点源；
//   - 多仓：Detail.SubURLs 全部展开；
//   - 期望集合中已消失的旧关联（多仓内容变化、父源改地址）→ 摘除父引用，无父则删。
//
// 返回是否发生实际变化。调用方需持有写锁。
func (s *Store) syncFlatsForSourceLocked(src model.Source) bool {
	changed := false
	expect := map[string]bool{}
	if src.Type == model.TypeMulti && src.Detail != nil {
		for _, su := range src.Detail.SubURLs {
			norm, err := parser.Normalize(su.URL)
			if err != nil {
				continue
			}
			expect[norm] = true
			name := su.Name
			if name == "" {
				name = src.Name
			}
			if s.upsertFlatLocked(src, norm, name, false) { // 多仓子仓：首次名称优先
				changed = true
			}
		}
	} else {
		norm, err := parser.Normalize(src.URL)
		if err != nil {
			return changed
		}
		expect[norm] = true
		if s.upsertFlatLocked(src, norm, src.Name, true) { // 单仓父源：名称/分组始终随父源同步
			changed = true
		}
	}

	if s.pruneFlatsLocked(src.ID, func(f model.FlatSource) bool {
		return containsID(f.ParentIDs, src.ID) && !expect[f.URL]
	}) {
		changed = true
	}
	return changed
}

// pruneFlatsLocked 摘除命中 drop 的站点源上的 parentID 引用；
// 无父引用且未隐藏的物理删除。隐藏项即使没有父引用也保留为墓碑，
// 避免父源再次同步时复活被删除的条目。返回是否发生实际变化。
func (s *Store) pruneFlatsLocked(parentID string, drop func(model.FlatSource) bool) bool {
	kept := s.data.Flats[:0]
	changed := false
	now := time.Now()
	for _, f := range s.data.Flats {
		if drop(f) {
			changed = true
			f.ParentIDs = removeID(f.ParentIDs, parentID)
			f.UpdatedAt = now
			if len(f.ParentIDs) == 0 && !f.Hidden {
				continue // 不再被任何父源引用 → 物理删除
			}
		}
		kept = append(kept, f)
	}
	if changed {
		s.data.Flats = kept
	}
	return changed
}

// removeSourceFlatRefsLocked 父源删除后摘除引用，返回是否发生变化
func (s *Store) removeSourceFlatRefsLocked(sourceID string) bool {
	return s.pruneFlatsLocked(sourceID, func(f model.FlatSource) bool {
		return containsID(f.ParentIDs, sourceID)
	})
}

// ResyncAllFlats 与全部父源对账（启动 / replace 导入后调用）
func (s *Store) ResyncAllFlats() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resyncAllFlatsLocked()
}

// resyncAllFlatsLocked 对账式重建站点源：以现有 Flats 为底，按父源期望集合
// 补齐新增、摘除孤儿，保留已有条目的 ID / 启用状态 / 名称 / 备注 / 探活结果。
// 只在内容真的变化时落盘，避免每次启动都重写 data.json 和刷掉用户修改。
func (s *Store) resyncAllFlatsLocked() bool {
	changed := false
	for i := range s.data.Sources {
		if s.syncFlatsForSourceLocked(s.data.Sources[i]) {
			changed = true
		}
	}
	// 兜底清理：引用的父源已全部消失的条目（replace 导入、手工改数据后会出现）
	idx := s.srcIndexLocked()
	dropped := false
	kept := make([]model.FlatSource, 0, len(s.data.Flats))
	for _, f := range s.data.Flats {
		if !f.Hidden && !hasAliveParent(f, idx) {
			dropped = true
			continue
		}
		kept = append(kept, f)
	}
	if dropped {
		s.data.Flats = kept
		changed = true
	}
	if changed {
		_ = s.flush()
	}
	return changed
}

func hasAliveParent(f model.FlatSource, idx map[string]model.Source) bool {
	for _, pid := range f.ParentIDs {
		if _, ok := idx[pid]; ok {
			return true
		}
	}
	return false
}
