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
			if fi, fj := statusRank(out[i].Status), statusRank(out[j].Status); fi != fj {
				return fi < fj
			}
		}
		return cmpFlatAvail(out[i], out[j]) < 0
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

// FlatGroups 所有站点源分组名（去重排序）；已删除（隐藏）的条目不参与，
// 否则分组下拉里会留下一个永远筛不出东西的空分组。
func (s *Store) FlatGroups() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	seen := map[string]bool{}
	for i := range s.data.Flats {
		if s.data.Flats[i].Hidden {
			continue
		}
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

// UpdateFlatCheckResult 站点源探活结果回写（连续失败/成功次数口径与父源一致）
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
			fp.OKStreak++
			fp.FailStreak = 0
			fp.LastOKAt = &now
		} else {
			fp.FailStreak++
			fp.OKStreak = 0
			fp.FailCount++
		}
		// 连续失败自动禁用：与父源同口径，回写层统一判断（含手动单条/批量检测）
		s.autoDisableLocked(&fp.Enabled, &fp.FailStreak, &fp.CheckMsg)
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
// 「已存在」先按原样精确匹配，没命中再按 parser.DedupKey 匹配一次：
// https://a.com/tv 与 https://a.com/tv/ 是同一条线路，必须合并成一条站点源
// （父引用累加），否则订阅里会下发两次同一个地址。
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
	idx := -1
	for i := range s.data.Flats {
		if s.data.Flats[i].URL == norm { // 绝大多数情况：零额外开销
			idx = i
			break
		}
	}
	if idx < 0 {
		key := parser.DedupKey(norm)
		for i := range s.data.Flats {
			if parser.DedupKey(s.data.Flats[i].URL) == key {
				idx = i
				break
			}
		}
	}
	if idx >= 0 {
		f := &s.data.Flats[idx]
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
		// 本地源的站点源始终可用（内容就在本服务上，无需探活）
		if parent.Type == model.TypeLocal && f.Status != model.StatusOK {
			t := time.Now()
			f.Status = model.StatusOK
			f.LastCheckAt = &t
			f.CheckMsg = "本地源，无需检测"
			changed = true
		}
		if changed {
			f.UpdatedAt = time.Now()
		}
		return changed
	}
	now := s.nowStrictLocked()
	if name == "" {
		name = parent.Name
	}
	flatStatus := model.StatusUnknown
	var flatLastCheck *time.Time
	flatMsg := ""
	if parent.Type == model.TypeLocal { // 本地源：直接可用，不参与检测
		flatStatus = model.StatusOK
		t := now
		flatLastCheck = &t
		flatMsg = "本地源，无需检测"
	}
	s.data.Flats = append(s.data.Flats, model.FlatSource{
		ID:             NewFlatID(),
		Name:           name,
		URL:            norm,
		ParentIDs:      []string{parent.ID},
		Enabled:        true,
		Group:          group,
		Sort:           parent.Sort,
		TimeoutSeconds: parent.TimeoutSeconds, // 探活超时随父源，子仓往往同样大
		NameAuto:  true,
		GroupAuto: true,
		Status:    flatStatus,
		LastCheckAt: flatLastCheck,
		CheckMsg:    flatMsg,
		CreatedAt: now,
		UpdatedAt: now,
	})
	return true
}

// desiredFlat 父源期望生成的一条站点源
type desiredFlat struct {
	URL       string // 已归一化
	Name      string
	ForceName bool // true=名称始终随父源同步（单仓），false=首次名称优先
}

// desiredFlats 计算父源期望生成的站点源集合。
//
// 第二个返回值 definitive 表示「这次能不能对账」：
//   - false：父源自身地址都归一化不了，此时返回空集合绝不代表「该源不该有站点源」，
//     调用方必须原样跳过（否则会把已有站点源全摘掉）；
//   - true + 空集合：确实不该有站点源（未定型 / 多仓缺子仓列表），
//     调用方应当把残留的旧关联清掉。
func desiredFlats(src model.Source) (list []desiredFlat, definitive bool) {
	switch {
	case src.Type == model.TypeMulti:
		// 还没拿到子仓列表（首次探活未成功 / 手动强制多仓）→ 先不生成，
		// 等 Detail 到位再由同步函数展开，避免把多仓地址当单仓下发
		if src.Detail == nil {
			return nil, true
		}
		out := make([]desiredFlat, 0, len(src.Detail.SubURLs))
		for _, su := range src.Detail.SubURLs {
			norm, err := parser.Normalize(su.URL)
			if err != nil {
				continue
			}
			name := su.Name
			if name == "" {
				name = src.Name
			}
			out = append(out, desiredFlat{URL: norm, Name: name})
		}
		return out, true
	case src.Type == model.TypeUnknown:
		// 未确认类型的源不进订阅
		return nil, true
	default:
		norm, err := parser.Normalize(src.URL)
		if err != nil {
			return nil, false
		}
		return []desiredFlat{{URL: norm, Name: src.Name, ForceName: true}}, true
	}
}

// syncFlatsForSourceLocked 父源变化后对齐其关联站点源（期望集合见 desiredFlats）；
// 期望集合中已消失的旧关联（多仓内容变化、父源改地址）→ 摘除父引用，无父则删。
//
// 返回是否发生实际变化。调用方需持有写锁。
func (s *Store) syncFlatsForSourceLocked(src model.Source) bool {
	changed := false
	want, definitive := desiredFlats(src)
	if !definitive {
		return false
	}
	expect := make(map[string]bool, len(want))
	for _, d := range want {
		expect[parser.DedupKey(d.URL)] = true
		if s.upsertFlatLocked(src, d.URL, d.Name, d.ForceName) {
			changed = true
		}
	}

	if s.pruneFlatsLocked(src.ID, func(f model.FlatSource) bool {
		return containsID(f.ParentIDs, src.ID) && !expect[parser.DedupKey(f.URL)]
	}) {
		changed = true
	}
	return changed
}

// RestoreReport 恢复结果，前端据此给用户看「到底恢复了几条、为什么恢复不了」
type RestoreReport struct {
	SourceID string `json:"sourceId"`
	Type     string `json:"type"`
	Restored int    `json:"restored"` // 从墓碑复活（用户之前删掉的）
	Created  int    `json:"created"`  // 新生成的
	Total    int    `json:"total"`    // 该源期望的站点源总数
	Alive    int    `json:"alive"`    // 恢复后该源可见的站点源条数
	Hint     string `json:"hint,omitempty"`
}

// RestoreSourceFlats 按父源重建它应有的站点源，并复活被删掉的条目。
//
// 解决「源列表里有、仓源里没有」里最难自查的一类：用户在多仓展开后误删了子仓，
// 该条目变成墓碑（Hidden），此后父源怎么同步都不会复活它，界面上下次看到的就是
// 「这个源明明启用了，订阅里却没有它」。这里显式清墓碑 + 对账，是用户可触发的补救。
func (s *Store) RestoreSourceFlats(id string) (RestoreReport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rep := RestoreReport{SourceID: id}
	idx := -1
	for i := range s.data.Sources {
		if s.data.Sources[i].ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return rep, fmt.Errorf("源不存在: %s", id)
	}
	src := s.data.Sources[idx]
	rep.Type = src.Type

	want, definitive := desiredFlats(src)
	if !definitive {
		rep.Hint = "该源地址无法解析，请检查地址是否正确"
		return rep, nil
	}
	if len(want) == 0 {
		switch {
		case src.Type == model.TypeUnknown:
			rep.Hint = "该源还没定型，分不清是单仓还是多仓，所以不会下发。请先「检测」，或在编辑里手动指定类型后再恢复"
		case src.Type == model.TypeMulti:
			rep.Hint = "多仓源还没解析出子仓地址（探活未成功或子仓列表为空）。请先「检测」，让它把子仓展开出来"
		default:
			rep.Hint = "该源没有可用的站点地址"
		}
		return rep, nil
	}
	rep.Total = len(want)

	// 恢复前该父源名下可见条数（用于区分「复活」和「新建」）
	before := 0
	for i := range s.data.Flats {
		if f := s.data.Flats[i]; !f.Hidden && containsID(f.ParentIDs, src.ID) {
			before++
		}
	}

	// 1) 复活墓碑：期望集合内、之前被删掉的条目
	now := time.Now()
	restorable := make(map[string]bool, len(want))
	for _, d := range want {
		restorable[parser.DedupKey(d.URL)] = true
	}
	for i := range s.data.Flats {
		f := &s.data.Flats[i]
		if !f.Hidden || !restorable[parser.DedupKey(f.URL)] {
			continue
		}
		f.Hidden = false
		f.Enabled = true
		f.UpdatedAt = now
		rep.Restored++
	}

	// 2) 对账补齐：缺少的建出来，父引用丢失的补回来
	s.syncFlatsForSourceLocked(src)

	// 3) 统计恢复后该父源名下可见的站点源
	for i := range s.data.Flats {
		f := s.data.Flats[i]
		if f.Hidden || !containsID(f.ParentIDs, src.ID) {
			continue
		}
		rep.Alive++
	}
	if grew := rep.Alive - before; grew > rep.Restored {
		rep.Created = grew - rep.Restored
	}
	return rep, s.flush()
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

// ---------- 存量数据修复 ----------

// mergeDuplicateFlatsLocked 合并历史数据里指向同一个地址的多条站点源。
//
// 旧版本只按 Normalize 后的字符串判重，而 Normalize 刻意不动尾斜杠，于是
// https://a.com/tv 与 https://a.com/tv/ 被存成两条（本地实测确实存在），
// 订阅里同一地址就下发两次、名字还各是各的。现在 upsert 已按 DedupKey 合并，
// 这里负责把存量也收拢：父引用并到最早那条，其余删除。
//
// 两条边界：
//   - 已隐藏（用户手动删过）的条目不参与合并、也不会被复活；
//   - 启用状态取「任一条启用即启用」——合并是为了去重，不该顺手把用户的线路关掉。
func (s *Store) mergeDuplicateFlatsLocked() bool {
	first := make(map[string]int, len(s.data.Flats))
	drop := make(map[int]bool)
	for i := range s.data.Flats {
		f := &s.data.Flats[i]
		if f.Hidden {
			continue
		}
		key := parser.DedupKey(f.URL)
		j, ok := first[key]
		if !ok {
			first[key] = i
			continue
		}
		keep := &s.data.Flats[j]
		for _, pid := range f.ParentIDs {
			if !containsID(keep.ParentIDs, pid) {
				keep.ParentIDs = append(keep.ParentIDs, pid)
			}
		}
		if f.Enabled {
			keep.Enabled = true
		}
		keep.UpdatedAt = time.Now()
		drop[i] = true
	}
	if len(drop) == 0 {
		return false
	}
	kept := make([]model.FlatSource, 0, len(s.data.Flats)-len(drop))
	for i := range s.data.Flats {
		if !drop[i] {
			kept = append(kept, s.data.Flats[i])
		}
	}
	s.data.Flats = kept
	return true
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
