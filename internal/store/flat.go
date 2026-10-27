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

// effectiveEnabledLocked 自身启用 && 任一父源启用
func (s *Store) effectiveEnabledLocked(f model.FlatSource) bool {
	if !f.Enabled || len(f.ParentIDs) == 0 {
		return false
	}
	for _, pid := range f.ParentIDs {
		for i := range s.data.Sources {
			if s.data.Sources[i].ID == pid && s.data.Sources[i].Enabled {
				return true
			}
		}
	}
	return false
}

// parentNamesLocked 父源名称列表（父源已删除的引用忽略）
func (s *Store) parentNamesLocked(f model.FlatSource) []string {
	names := make([]string, 0, len(f.ParentIDs))
	for _, pid := range f.ParentIDs {
		for i := range s.data.Sources {
			if s.data.Sources[i].ID == pid {
				names = append(names, s.data.Sources[i].Name)
				break
			}
		}
	}
	return names
}

// computeFlatLocked 回填运行时字段（有效启用、父源名称）
func (s *Store) computeFlatLocked(f *model.FlatSource) {
	f.EffectiveEnabled = s.effectiveEnabledLocked(*f)
	f.ParentNames = s.parentNamesLocked(*f)
}

// ListFlats 过滤 + 排序后返回副本
func (s *Store) ListFlats(f FlatFilter) []model.FlatSource {
	s.mu.RLock()
	out := make([]model.FlatSource, 0, len(s.data.Flats))
	for i := range s.data.Flats {
		fc := cloneFlat(s.data.Flats[i])
		s.computeFlatLocked(&fc)
		out = append(out, fc)
	}
	s.mu.RUnlock()

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

	if f.SortBy != "" {
		// 表头排序优先
		sortByHeader(out, func(a, b model.FlatSource) int { return cmpFlat(a, b, f.SortBy) }, f.SortDir == "desc", flatFallback)
	} else {
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

// GetFlat 单条副本
func (s *Store) GetFlat(id string) (model.FlatSource, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for i := range s.data.Flats {
		if s.data.Flats[i].ID == id {
			fc := cloneFlat(s.data.Flats[i])
			s.computeFlatLocked(&fc)
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
			s.computeFlatLocked(&updated)
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
	for i := range s.data.Flats {
		if s.data.Flats[i].ID == id {
			updated := cloneFlat(s.data.Flats[i])
			fn(&updated)
			s.computeFlatLocked(&updated)
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

// DeleteFlats 删除站点源，返回实际删除数
func (s *Store) DeleteFlats(ids []string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deleteFlatsLocked(ids)
}

func (s *Store) deleteFlatsLocked(ids []string) int {
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	kept := s.data.Flats[:0]
	n := 0
	for _, x := range s.data.Flats {
		if want[x.ID] {
			n++
			continue
		}
		kept = append(kept, x)
	}
	if n > 0 {
		s.data.Flats = kept
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
		return s.DeleteFlats(ids), nil
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
// GroupAuto 时同步父源分组。
func (s *Store) upsertFlatLocked(parent model.Source, rawURL, name string, forceName bool) {
	norm, err := parser.Normalize(rawURL)
	if err != nil {
		return
	}
	group := parent.Group
	if group == "" {
		group = "默认"
	}
	for i := range s.data.Flats {
		if s.data.Flats[i].URL != norm {
			continue
		}
		f := &s.data.Flats[i]
		if !containsID(f.ParentIDs, parent.ID) {
			f.ParentIDs = append(f.ParentIDs, parent.ID)
			f.UpdatedAt = time.Now()
		}
		if name != "" && (f.Name == "" || (forceName && f.NameAuto)) {
			if f.Name != name {
				f.Name = name
			}
			f.UpdatedAt = time.Now()
		}
		if f.GroupAuto && f.Group != group {
			f.Group = group
			f.UpdatedAt = time.Now()
		}
		return
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
}

// syncFlatsForSourceLocked 父源变化后重建其关联站点源：
//   - 单仓/未知：自身地址生成一条站点源；
//   - 多仓：Detail.SubURLs 全部展开；
//   - 期望集合中已消失的旧关联（多仓内容变化、父源改地址）→ 摘除父引用，无父则删除。
//
// 调用方需持有写锁。
func (s *Store) syncFlatsForSourceLocked(src model.Source) {
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
			s.upsertFlatLocked(src, norm, name, false) // 多仓子仓：首次名称优先
		}
	} else {
		norm, err := parser.Normalize(src.URL)
		if err != nil {
			return
		}
			expect[norm] = true
			s.upsertFlatLocked(src, norm, src.Name, true) // 单仓父源：名称/分组始终随父源同步
	}

	kept := s.data.Flats[:0]
	for _, f := range s.data.Flats {
		if containsID(f.ParentIDs, src.ID) && !expect[f.URL] {
			f.ParentIDs = removeID(f.ParentIDs, src.ID)
			f.UpdatedAt = time.Now()
			if len(f.ParentIDs) == 0 {
				continue // 不再被任何父源引用，删除
			}
		}
		kept = append(kept, f)
	}
	s.data.Flats = kept
}

// removeSourceFlatRefsLocked 父源删除后摘除引用
func (s *Store) removeSourceFlatRefsLocked(sourceID string) {
	kept := s.data.Flats[:0]
	for _, f := range s.data.Flats {
		if containsID(f.ParentIDs, sourceID) {
			f.ParentIDs = removeID(f.ParentIDs, sourceID)
			if len(f.ParentIDs) == 0 {
				continue
			}
		}
		kept = append(kept, f)
	}
	s.data.Flats = kept
}

// ResyncAllFlats 全量重建站点源（启动 / replace 导入后调用）
func (s *Store) ResyncAllFlats() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resyncAllFlatsLocked()
}

func (s *Store) resyncAllFlatsLocked() {
	s.data.Flats = nil
	for i := range s.data.Sources {
		s.syncFlatsForSourceLocked(s.data.Sources[i])
	}
	_ = s.flush()
}
