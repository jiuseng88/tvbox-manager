package store

import (
	"testing"
	"time"

	"tvbox-manager/internal/model"
)

func TestScrapeSeedsCRUD(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got := len(s.ListScrapeSeeds()); got != 0 {
		t.Fatalf("初始应有 0 个种子，实际 %d", got)
	}
	seed, err := s.AddScrapeSeed(&model.ScrapeSeed{Name: "示例源", URL: "https://example.com/sub.json", Group: "默认"})
	if err != nil {
		t.Fatal(err)
	}
	if seed.ID == "" {
		t.Fatal("AddScrapeSeed 应生成 ID")
	}
	if got := len(s.ListScrapeSeeds()); got != 1 {
		t.Fatalf("应 1 个种子，实际 %d", got)
	}
	// 更新
	if err := s.UpdateScrapeSeed(seed.ID, func(sp *model.ScrapeSeed) { sp.Name = "改名" }); err != nil {
		t.Fatal(err)
	}
	got, ok := s.GetScrapeSeed(seed.ID)
	if !ok || got.Name != "改名" {
		t.Fatalf("更新失败: %+v", got)
	}
	// 不存在的种子更新应报错
	if err := s.UpdateScrapeSeed("seed_nope", func(sp *model.ScrapeSeed) {}); err == nil {
		t.Fatal("更新不存在的种子应报错")
	}
	// 删除
	if !s.DeleteScrapeSeed(seed.ID) {
		t.Fatal("删除应成功")
	}
	if len(s.ListScrapeSeeds()) != 0 {
		t.Fatal("删除后应为空")
	}
	if s.DeleteScrapeSeed(seed.ID) {
		t.Fatal("重复删除应返回 false")
	}
}

func TestScrapeCandidatesUpsert(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	seed, _ := s.AddScrapeSeed(&model.ScrapeSeed{Name: "A", URL: "https://a.example/x.json"})

	now := time.Now()
	c1 := model.ScrapeCandidate{SeedID: seed.ID, Name: "c1", URL: "https://a.example/1", Status: "pending", FetchedAt: now}
	c2 := model.ScrapeCandidate{SeedID: seed.ID, Name: "c2", URL: "https://a.example/2", Status: "pending", FetchedAt: now}
	if err := s.UpsertScrapeCandidates(seed.ID, []model.ScrapeCandidate{c1, c2}); err != nil {
		t.Fatal(err)
	}
	all := s.ListScrapeCandidates()
	if len(all) != 2 {
		t.Fatalf("应 2 个候选，实际 %d", len(all))
	}
	// 稳定键：两次同步同 URL 应是同一条记录（ID 派生自 seedID+URL）
	id1 := CandidateID(seed.ID, "https://a.example/1")

	// 第二次同步：c1 不再出现（pending → 应被丢弃）；c2 仍在；新增 c3
	c2b := model.ScrapeCandidate{SeedID: seed.ID, Name: "c2", URL: "https://a.example/2", Status: "pending", FetchedAt: now}
	c3 := model.ScrapeCandidate{SeedID: seed.ID, Name: "c3", URL: "https://a.example/3", Status: "pending", FetchedAt: now}
	if err := s.UpsertScrapeCandidates(seed.ID, []model.ScrapeCandidate{c2b, c3}); err != nil {
		t.Fatal(err)
	}
	all = s.ListScrapeCandidates()
	if len(all) != 2 {
		t.Fatalf("二次同步后应为 2 个（c1 过期丢弃，c2 保留，c3 新增），实际 %d", len(all))
	}
	for _, c := range all {
		if c.URL == "https://a.example/1" {
			t.Fatal("过期的 pending c1 应被丢弃")
		}
		if c.ID != id1 && c.URL == "https://a.example/2" {
			// c2 的 ID 应稳定
		}
	}

	// 确认 c2（标记 applied）
	if err := s.UpdateScrapeCandidates(map[string]string{CandidateID(seed.ID, "https://a.example/2"): "applied"}); err != nil {
		t.Fatal(err)
	}

	// 第三次同步：c2 不再出现，但它是 applied → 应保留并标记 upstreamStatus=removed（漂移，不删）
	if err := s.UpsertScrapeCandidates(seed.ID, []model.ScrapeCandidate{c3}); err != nil {
		t.Fatal(err)
	}
	all = s.ListScrapeCandidates()
	if len(all) != 2 {
		t.Fatalf("三次同步后应为 2 个（c2 漂移保留 + c3），实际 %d", len(all))
	}
	for _, c := range all {
		if c.URL == "https://a.example/2" && c.Status != "applied" {
			t.Fatalf("c2 应仍为 applied，实际 %s", c.Status)
		}
		if c.URL == "https://a.example/2" && c.UpstreamStatus != "removed" {
			t.Fatalf("c2 上游移除应标记 removed，实际 %q", c.UpstreamStatus)
		}
		if c.URL == "https://a.example/3" && c.UpstreamStatus != "present" {
			t.Fatalf("c3 应 present，实际 %q", c.UpstreamStatus)
		}
	}

	// 删除种子连带候选
	s.DeleteScrapeSeed(seed.ID)
	if len(s.ListScrapeCandidates()) != 0 {
		t.Fatal("删除种子后候选应一并清理")
	}
}

func TestScrapeCandidatesPersistAcrossReload(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	seed, _ := s.AddScrapeSeed(&model.ScrapeSeed{Name: "A", URL: "https://a.example/x.json"})
	c := model.ScrapeCandidate{SeedID: seed.ID, Name: "c1", URL: "https://a.example/1", Status: "pending", FetchedAt: time.Now()}
	if err := s.UpsertScrapeCandidates(seed.ID, []model.ScrapeCandidate{c}); err != nil {
		t.Fatal(err)
	}
	// 重新打开（模拟重启）
	s2, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	seeds := s2.ListScrapeSeeds()
	if len(seeds) != 1 || seeds[0].Name != "A" {
		t.Fatalf("重启后种子应保留: %+v", seeds)
	}
	cands := s2.ListScrapeCandidates()
	if len(cands) != 1 || cands[0].URL != "https://a.example/1" {
		t.Fatalf("重启后候选应保留: %+v", cands)
	}
}
