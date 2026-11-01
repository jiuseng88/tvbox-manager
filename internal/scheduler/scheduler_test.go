package scheduler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"tvbox-manager/internal/model"
	"tvbox-manager/internal/store"
)

// 定时刮削回调：注册后可直接调用（不触发真实定时器与网络）
func TestScrapeSyncCallback(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New 失败: %v", err)
	}
	cfg := model.DefaultConfig()
	cfg.CheckIntervalMinutes = 600
	sch := Start(st, &cfg)
	defer sch.Shutdown()

	called := false
	sch.SetScrapeSync(func(ctx context.Context) { called = true })

	sch.scrapeMu.Lock()
	fn := sch.scrapeFn
	sch.scrapeMu.Unlock()
	if fn == nil {
		t.Fatal("回调未注册")
	}
	fn(context.Background())
	if !called {
		t.Fatal("回调未被调用")
	}
}

// scrapeInterval：0 = 关闭（超长间隔），>0 = 按小时
func TestScrapeInterval(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New 失败: %v", err)
	}
	cfg := model.DefaultConfig()
	cfg.ScrapeIntervalHours = 0
	sch := Start(st, &cfg)
	defer sch.Shutdown()
	if d := sch.scrapeInterval(); d != 365*24*time.Hour {
		t.Fatalf("0 小时应为关闭（超长间隔），实际 %v", d)
	}

	cfg.ScrapeIntervalHours = 6
	sch.Reload(&cfg)
	if d := sch.scrapeInterval(); d != 6*time.Hour {
		t.Fatalf("6 小时应返回 6h，实际 %v", d)
	}
}

// 连续失败 N 次自动禁用：父源与站点源同口径（站点源同地址复用父源本轮结果）
func TestAutoDisableAfterConsecutiveFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	}))
	defer srv.Close()

	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New 失败: %v", err)
	}
	src := model.Source{Name: "坏源", URL: srv.URL + "/1.json", Type: model.TypeSingle, Enabled: true}
	if err := st.Add(&src); err != nil {
		t.Fatal(err)
	}

	cfg := model.DefaultConfig()
	cfg.CheckIntervalMinutes = 600
	cfg.CheckTimeoutSeconds = 3
	cfg.CheckConcurrency = 2
	cfg.AutoDisableAfterFails = 2
	// 自动禁用阈值现在以 store 的配置为准（回写层统一判断），要同步进 store
	if err := st.UpdateConfig(cfg); err != nil {
		t.Fatal(err)
	}

	sch := Start(st, &cfg)
	defer sch.Shutdown()

	// 第 1 轮：失败 1 次，未到阈值
	sch.RunAll("test")
	if s, _ := st.Get(src.ID); !s.Enabled {
		t.Fatal("失败 1 次不应禁用父源")
	}
	if f := st.ListFlats(store.FlatFilter{})[0]; !f.Enabled {
		t.Fatal("失败 1 次不应禁用站点源")
	}

	// 第 2 轮：连续失败 2 次 → 父源与站点源都应自动禁用
	sch.RunAll("test")
	s, _ := st.Get(src.ID)
	if s.Enabled {
		t.Errorf("连续失败 2 次应自动禁用父源，checkMsg=%q", s.CheckMsg)
	}
	flats := st.ListFlats(store.FlatFilter{})
	if len(flats) != 1 {
		t.Fatalf("站点源条数错误: %d", len(flats))
	}
	if flats[0].Enabled {
		t.Errorf("连续失败 2 次应自动禁用站点源，checkMsg=%q", flats[0].CheckMsg)
	}
	if flats[0].FailStreak != 2 {
		t.Errorf("站点源连续失败次数应为 2，实际 %d", flats[0].FailStreak)
	}
	// 禁用后不再下发
	if n := len(st.EnabledFlats()); n != 0 {
		t.Errorf("自动禁用后仍出现在订阅输出: %d", n)
	}
}

// 成功一次即清零：中途恢复过的源不该因为历史累计失败被禁用
func TestNoAutoDisableWhenRecovered(t *testing.T) {
	var fail bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"sites":[{"key":"a","name":"A","type":0,"api":"csp_A","searchable":1,"quickSearch":1,"filterable":0}]}`))
	}))
	defer srv.Close()

	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New 失败: %v", err)
	}
	src := model.Source{Name: "源", URL: srv.URL + "/1.json", Type: model.TypeSingle, Enabled: true}
	if err := st.Add(&src); err != nil {
		t.Fatal(err)
	}

	cfg := model.DefaultConfig()
	cfg.CheckIntervalMinutes = 600
	cfg.CheckTimeoutSeconds = 5
	cfg.CheckConcurrency = 2
	cfg.AutoDisableAfterFails = 2
	// 自动禁用阈值现在以 store 的配置为准（回写层统一判断），要同步进 store
	if err := st.UpdateConfig(cfg); err != nil {
		t.Fatal(err)
	}

	sch := Start(st, &cfg)
	defer sch.Shutdown()

	fail = true
	sch.RunAll("test") // 连续失败 1
	fail = false
	sch.RunAll("test") // 成功 → 连续失败清零
	fail = true
	sch.RunAll("test") // 连续失败 1（累计已 2，按累计口径这里就会被禁用）
	if s, _ := st.Get(src.ID); !s.Enabled {
		t.Fatalf("中途成功过就不该按累计失败数禁用（failCount=%d streak=%d）", s.FailCount, s.FailStreak)
	}
	sch.RunAll("test") // 连续失败 2 → 禁用
	if s, _ := st.Get(src.ID); s.Enabled {
		t.Errorf("连续失败 2 次应禁用（streak=%d）", s.FailStreak)
	}
}
