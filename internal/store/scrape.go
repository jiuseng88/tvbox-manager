package store

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"tvbox-manager/internal/model"
)

// ---------- 自动刮削：种子清单与候选区（PRD A-06） ----------
//
// 候选区语义：刮削结果先入候选（pending），用户确认后才导入为源；
// 同步某个种子时会整批替换该种子的旧候选（含已 applied 的历史），
// 候选区始终是「最近一次同步 + 未确认」的活动窗口，不会无限累积。

// NewSeedID 生成种子 ID（与源 ID 前缀区分，避免混淆）
func NewSeedID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return "seed_" + hex.EncodeToString(b)
}

// NewCandidateID 生成候选 ID
func NewCandidateID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return "cand_" + hex.EncodeToString(b)
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

// SetScrapeCandidates 整批替换某个种子的候选（同步结果写入）
func (s *Store) SetScrapeCandidates(seedID string, cands []model.ScrapeCandidate) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.data.ScrapeCandidates[:0]
	for _, c := range s.data.ScrapeCandidates {
		if c.SeedID == seedID {
			continue
		}
		kept = append(kept, c)
	}
	s.data.ScrapeCandidates = append(kept, cands...)
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
// 复用 AddLiveSources 同一套 liveKey 去重口径，避免同步标记与导入判断不一致。
func (s *Store) LiveSourceExists(rawURL string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.findLiveByKeyLocked(liveKey(strings.TrimSpace(rawURL))) >= 0
}
