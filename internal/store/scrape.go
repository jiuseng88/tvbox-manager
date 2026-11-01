package store

import (
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"tvbox-manager/internal/model"
	"tvbox-manager/internal/parser"
)

// ---------- 自动刮削：种子清单与候选区（PRD A-06 / A-07） ----------

// 候选区语义（重建后）：
//   - 候选 ID 由 (SeedID + 归一化URL) 派生，稳定可重入；
//   - 同步某个种子时按稳定键 upsert：已确认（applied/skipped）状态保留，pending 过期清除，
//     上游已移除的已确认候选标记 upstreamStatus=removed（漂移，不自动删除）；
//   - 候选区始终是「最近发现 + 你的确认」，不再被整批覆盖。

// NewSeedID 生成种子 ID（与源 ID 前缀区分，避免混淆）
func NewSeedID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return "seed_" + hex.EncodeToString(b)
}

// CandidateID 由 (SeedID + 归一化URL) 派生稳定 ID：同来源同地址多次同步保持同一条记录。
func CandidateID(seedID, normalizedURL string) string {
	h := sha1.Sum([]byte(seedID + "\x00" + strings.ToLower(strings.TrimSpace(normalizedURL))))
	return "cand_" + hex.EncodeToString(h[:])[:12]
}

// AddScrapeSeed 新增种子（调用方负责归一化与查重），生成 ID 并回填，落盘。
func (s *Store) AddScrapeSeed(seed *model.ScrapeSeed) (model.ScrapeSeed, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if seed.ID == "" {
		seed.ID = NewSeedID()
	}
	now := s.nowStrictLocked()
	seed.CreatedAt = now
	if seed.Group == "" {
		seed.Group = "默认"
	}
	s.data.ScrapeSeeds = append(s.data.ScrapeSeeds, *seed)
	if err := s.flush(); err != nil {
		return model.ScrapeSeed{}, err
	}
	return *seed, nil
}

// ListScrapeSeeds 全量副本
func (s *Store) ListScrapeSeeds() []model.ScrapeSeed {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]model.ScrapeSeed, 0, len(s.data.ScrapeSeeds))
	for i := range s.data.ScrapeSeeds {
		out = append(out, s.data.ScrapeSeeds[i])
	}
	return out
}

// GetScrapeSeed 单条副本
func (s *Store) GetScrapeSeed(id string) (model.ScrapeSeed, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for i := range s.data.ScrapeSeeds {
		if s.data.ScrapeSeeds[i].ID == id {
			return s.data.ScrapeSeeds[i], true
		}
	}
	return model.ScrapeSeed{}, false
}

// UpdateScrapeSeed 修改并落盘
func (s *Store) UpdateScrapeSeed(id string, fn func(*model.ScrapeSeed)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.data.ScrapeSeeds {
		if s.data.ScrapeSeeds[i].ID == id {
			updated := s.data.ScrapeSeeds[i]
			fn(&updated)
			s.data.ScrapeSeeds[i] = updated
			return s.flush()
		}
	}
	return fmt.Errorf("种子不存在: %s", id)
}

// DeleteScrapeSeed 删除种子并连带删除其全部候选（防止候选区残留孤儿）
func (s *Store) DeleteScrapeSeed(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	seeds := s.data.ScrapeSeeds[:0]
	found := false
	for _, x := range s.data.ScrapeSeeds {
		if x.ID == id {
			found = true
			continue
		}
		seeds = append(seeds, x)
	}
	if !found {
		return false
	}
	s.data.ScrapeSeeds = seeds
	cands := s.data.ScrapeCandidates[:0]
	for _, c := range s.data.ScrapeCandidates {
		if c.SeedID == id {
			continue
		}
		cands = append(cands, c)
	}
	s.data.ScrapeCandidates = cands
	_ = s.flush()
	return true
}

// UpsertScrapeCandidates 按稳定键 (SeedID + 归一化URL) 合并候选：
//   - 新项 → pending；
//   - 已存在（无论状态）→ 更新易变字段，保留 Status / AppliedAt，标记 upstreamStatus=present；
//   - 此前 applied/skipped 但本次发现集不再出现 → 标记 upstreamStatus=removed（漂移，保留记录，不删除）；
//   - 此前 pending 但本次不再出现 → 视为过期，丢弃。
//
// 调用方应传入本次同步该种子的「全部」发现项；不在其中的旧项按上述规则处理。
func (s *Store) UpsertScrapeCandidates(seedID string, incoming []model.ScrapeCandidate) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	kept := make([]model.ScrapeCandidate, 0, len(s.data.ScrapeCandidates))
	existing := make(map[string]*model.ScrapeCandidate) // 本种子已有候选，按 URL 索引
	for i := range s.data.ScrapeCandidates {
		c := &s.data.ScrapeCandidates[i]
		if c.SeedID != seedID {
			kept = append(kept, *c)
			continue
		}
		existing[c.URL] = c
	}

	merged := make([]model.ScrapeCandidate, 0, len(kept)+len(incoming))
	merged = append(merged, kept...)

	for _, in := range incoming {
		key := strings.ToLower(strings.TrimSpace(in.URL))
		if ex, ok := existing[key]; ok {
			// 保留确认状态，刷新易变字段
			ex.Name = in.Name
			ex.Target = in.Target
			ex.Kind = in.Kind
			ex.Dup = in.Dup
			ex.Quality = in.Quality
			ex.Summary = in.Summary
			ex.FP = in.FP
			ex.DiscoveredDepth = in.DiscoveredDepth
			ex.ParentURL = in.ParentURL
			ex.FetchedAt = in.FetchedAt
			ex.LastSeenAt = in.LastSeenAt
			ex.UpstreamStatus = "present"
			merged = append(merged, *ex)
			delete(existing, key)
			continue
		}
		in.ID = CandidateID(seedID, in.URL)
		in.SeedID = seedID
		in.Status = "pending"
		in.UpstreamStatus = "present"
		merged = append(merged, in)
	}

	// 残留的旧候选（本次发现集已无）：pending 丢弃；已确认标记漂移保留
	for _, ex := range existing {
		if ex.Status == "pending" {
			continue
		}
		ex.UpstreamStatus = "removed"
		merged = append(merged, *ex)
	}

	s.data.ScrapeCandidates = merged
	return s.flush()
}

// ListScrapeCandidates 全部候选，按抓取时间升序（pending 在前）
func (s *Store) ListScrapeCandidates() []model.ScrapeCandidate {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]model.ScrapeCandidate, 0, len(s.data.ScrapeCandidates))
	for i := range s.data.ScrapeCandidates {
		out = append(out, s.data.ScrapeCandidates[i])
	}
	sort.Slice(out, func(i, j int) bool { return out[i].FetchedAt.Before(out[j].FetchedAt) })
	return out
}

// UpdateScrapeCandidates 批量回写候选状态（id → status：applied / skipped）
func (s *Store) UpdateScrapeCandidates(updates map[string]string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	for i := range s.data.ScrapeCandidates {
		if st, ok := updates[s.data.ScrapeCandidates[i].ID]; ok {
			s.data.ScrapeCandidates[i].Status = st
			if st == "applied" {
				now := time.Now()
				s.data.ScrapeCandidates[i].AppliedAt = &now
			}
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return s.flush()
}

// TouchScrapeSeedSync 记录某种子最近一次同步结果（不落盘之外的快照）
func (s *Store) TouchScrapeSeedSync(id, msg string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.data.ScrapeSeeds {
		if s.data.ScrapeSeeds[i].ID == id {
			s.data.ScrapeSeeds[i].LastSyncAt = &at
			s.data.ScrapeSeeds[i].LastSyncMsg = msg
			return
		}
	}
}

// LiveSourceExists 是否存在同 key 的直播源（直播刮削同步时标记 dup 用）。
func (s *Store) LiveSourceExists(rawURL string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.findLiveByKeyLocked(liveKey(strings.TrimSpace(rawURL))) >= 0
}

// ---------- 批量写入（apply 用，单次 flush） ----------

// AddManySources 批量追加源，全程只 flush 一次（解决逐条 Add 的 N 次写盘问题）。
// 返回实际新增条数。调用方传入的源应已完成归一化与查重。
func (s *Store) AddManySources(srcs []model.Source) (int, error) {
	if len(srcs) == 0 {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for i := range srcs {
		src := srcs[i]
		if src.ID == "" {
			src.ID = NewID()
		}
		now := s.nowStrictLocked()
		if src.CreatedAt.IsZero() {
			src.CreatedAt = now
		}
		src.UpdatedAt = now
		if src.Group == "" {
			src.Group = "默认"
		}
		if src.Status == "" {
			src.Status = model.StatusUnknown
		}
		if src.Type == "" {
			src.Type = model.TypeUnknown
		}
		s.data.Sources = append(s.data.Sources, src)
		s.syncFlatsForSourceLocked(src)
		n++
	}
	if err := s.flush(); err != nil {
		return 0, err
	}
	return n, nil
}

// LiveAddItem 直播源批量导入条目
type LiveAddItem struct {
	Name           string
	RawURL         string
	Group          string
	Remark         string
	Enabled        bool
	TimeoutSeconds int
	SeedID         string
	DiscoveredAt   *time.Time
	UpstreamFP     string
}

// AddManyLiveSources 批量追加直播源，单次 flush。已存在（含墓碑态）按 skip 处理。
// 返回新增 ID 列表与跳过条数。
func (s *Store) AddManyLiveSources(items []LiveAddItem) (added []string, skipped int, err error) {
	if len(items) == 0 {
		return nil, 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, it := range items {
		raw := strings.TrimSpace(it.RawURL)
		if raw == "" {
			skipped++
			continue
		}
		if s.findLiveByKeyLocked(liveKey(raw)) >= 0 {
			skipped++ // OnDuplicate skip：含墓碑恢复态一律跳过
			continue
		}
		now := time.Now()
		ls := model.LiveSource{
			ID:             NewLiveID(),
			Name:           strings.TrimSpace(it.Name),
			URL:            raw,
			Group:          defaultLiveGroup(it.Group),
			Remark:         strings.TrimSpace(it.Remark),
			Enabled:        it.Enabled,
			Origin:         model.LiveOriginManual,
			TimeoutSeconds: it.TimeoutSeconds,
			Status:         model.StatusUnknown,
			CreatedAt:      now,
			UpdatedAt:      now,
			SeedID:         it.SeedID,
			DiscoveredAt:   it.DiscoveredAt,
			UpstreamFP:     it.UpstreamFP,
		}
		if ls.Name == "" {
			ls.Name = liveURLStem(raw)
		}
		s.data.LiveSources = append(s.data.LiveSources, ls)
		added = append(added, ls.ID)
	}
	if len(added) > 0 {
		if err = s.flush(); err != nil {
			return added, skipped, err
		}
	}
	return added, skipped, nil
}

// ReconcileDrift 漂移对账：比较某种子已导入源与本次发现集（传入的是本次发现的原始 URL 列表）。
// 已导入但发现集不再出现且仍启用 → 标记 UpstreamRemoved=true（不删除）；
// 此前被标记移除、本次重新出现 → 清除标记。返回 (新移除, 已恢复) 计数。
func (s *Store) ReconcileDrift(seedID string, sourceURLs, liveURLs []string) (removed, restored int) {
	sourceSet := make(map[string]bool, len(sourceURLs))
	for _, u := range sourceURLs {
		key := strings.ToLower(strings.TrimSpace(u))
		if norm, err := parser.Normalize(u); err == nil {
			key = strings.ToLower(norm)
		}
		sourceSet[key] = true
	}
	liveSet := make(map[string]bool, len(liveURLs))
	for _, u := range liveURLs {
		liveSet[liveKey(u)] = true
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	changed := false
	for i := range s.data.Sources {
		src := &s.data.Sources[i]
		if src.SeedID != seedID {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(src.URL))
		if norm, err := parser.Normalize(src.URL); err == nil {
			key = strings.ToLower(norm)
		}
		present := sourceSet[key]
		if src.UpstreamRemoved && present {
			src.UpstreamRemoved = false
			src.UpdatedAt = now
			changed = true
			restored++
		} else if !src.UpstreamRemoved && !present && src.Enabled {
			src.UpstreamRemoved = true
			src.UpdatedAt = now
			changed = true
			removed++
		}
	}
	for i := range s.data.LiveSources {
		ls := &s.data.LiveSources[i]
		if ls.SeedID != seedID || ls.Hidden {
			continue
		}
		present := liveSet[liveKey(ls.URL)]
		if ls.UpstreamRemoved && present {
			ls.UpstreamRemoved = false
			ls.UpdatedAt = now
			changed = true
			restored++
		} else if !ls.UpstreamRemoved && !present && ls.Enabled {
			ls.UpstreamRemoved = true
			ls.UpdatedAt = now
			changed = true
			removed++
		}
	}
	if changed {
		_ = s.flush()
	}
	return removed, restored
}
