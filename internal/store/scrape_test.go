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

func TestScrapeCandidatesReplaceAndStatus(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	seed, _ := s.AddScrapeSeed(&model.ScrapeSeed{Name: "A", URL: "https://a.example/x.json"})
	now := time.Now()
	cands := []model.ScrapeCandidate{
		{ID: NewCandidateID(), SeedID: seed.ID, Name: "c1", URL: "https://a.example/1", Status: "pending", FetchedAt: now},
		{ID: NewCandidateID(), SeedID: seed.ID, Name: "c2", URL: "https://a.example/2", Status: "pending", FetchedAt: now},
	}
	if err := s.SetScrapeCandidates(seed.ID, cands); err != nil {
		t.Fatal(err)
	}
	if got := len(s.ListScrapeCandidates()); got != 2 {
		t.Fatalf("应 2 个候选，实际 %d", got)
	}
	// 整批替换：第二次同步只留新的（旧候选全部清掉）
	c2 := model.ScrapeCandidate{ID: NewCandidateID(), SeedID: seed.ID, Name: "c3", URL: "https://a.example/3", Status: "pending", FetchedAt: now}
	if err := s.SetScrapeCandidates(seed.ID, []model.ScrapeCandidate{c2}); err != nil {
		t.Fatal(err)
	}
	if got := len(s.ListScrapeCandidates()); got != 1 {
		t.Fatalf("整批替换后应 1 个候选，实际 %d", got)
	}
	// 状态回写
	if err := s.UpdateScrapeCandidates(map[string]string{c2.ID: "applied"}); err != nil {
		t.Fatal(err)
	}
	if got := s.ListScrapeCandidates()[0]; got.Status != "applied" {
		t.Fatalf("状态应为 applied，实际 %s", got.Status)
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
	c := model.ScrapeCandidate{ID: NewCandidateID(), SeedID: seed.ID, Name: "c1", URL: "https://a.example/1", Status: "pending", FetchedAt: time.Now()}
	if err := s.SetScrapeCandidates(seed.ID, []model.ScrapeCandidate{c}); err != nil {
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
