package store

import (
	"testing"

	"tvbox-manager/internal/model"
)

func TestLiveAddDedup(t *testing.T) {
	st := newTestStore(t)
	a, err := st.AddLiveSource("综合", "http://a.example/live.txt", "", "", true, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddLiveSource("综合2", "http://a.example/live.txt/", "", "", true, 0); err == nil {
		t.Fatal("尾斜杠等价地址应被去重拒绝")
	}
	// 带请求头参数的同一地址也算重复
	if _, err := st.AddLiveSource("综合3", "http://a.example/live.txt|User-Agent=x", "", "", true, 0); err == nil {
		t.Fatal("同地址带 | 头应被去重拒绝")
	}
	if a.Origin != model.LiveOriginManual || a.Group != "默认" || a.Name != "综合" {
		t.Fatalf("字段默认值错误: %+v", a)
	}
}

func TestLiveBatchAddAndDelete(t *testing.T) {
	st := newTestStore(t)
	items := []model.LiveSource{
		{Name: "A", URL: "http://a/1.m3u"},
		{Name: "B", URL: "http://b/1.m3u"},
		{Name: "C", URL: "http://a/1.m3u"}, // 重复
	}
	ids, skipped := st.AddLiveSources(items)
	if len(ids) != 2 || len(skipped) != 1 {
		t.Fatalf("应新增 2 跳过 1，实际 %d/%d", len(ids), len(skipped))
	}
	if n := st.DeleteLiveSources(ids); n != 2 {
		t.Fatalf("删除数错误: %d", n)
	}
	// manual 源删除是真删
	if _, ok := st.GetLiveSource(ids[0]); ok {
		t.Fatal("manual 源删除后不应存在")
	}
}

func TestLiveSyncFromParent(t *testing.T) {
	st := newTestStore(t)
	src := model.Source{Name: "肥猫", URL: "http://example.com/1.json", Enabled: true, Type: model.TypeSingle}
	if err := st.Add(&src); err != nil {
		t.Fatal(err)
	}

	// 探活成功，带回 lives → 应生成 auto 直播源
	src.Detail = &model.Detail{LiveSites: []model.LiveSiteEntry{
		{Name: "综合直播", URL: "http://live.example/a.m3u"},
		{Name: "地方频道", URL: "http://live.example/b.txt"},
	}}
	st.SyncLiveSourcesForSource(src)

	list, total := st.ListLiveSourcesPage(LiveFilter{})
	if total != 2 {
		t.Fatalf("应同步出 2 条直播源，实际 %d", total)
	}
	found := false
	for _, x := range list {
		if x.URL == "http://live.example/a.m3u" {
			found = true
			if x.Origin != model.LiveOriginAuto || len(x.ParentIDs) != 1 || x.Name != "综合直播" {
				t.Fatalf("auto 源字段错误: %+v", x)
			}
		}
	}
	if !found {
		t.Fatal("未同步到直播源")
	}

	// 用户改名 → 同步不应覆盖
	lv, _ := st.ListLiveSourcesPage(LiveFilter{})
	var target model.LiveSource
	for _, x := range lv {
		if x.URL == "http://live.example/a.m3u" {
			target = x
		}
	}
	_ = st.UpdateLiveSource(target.ID, func(x *model.LiveSource) { x.Name = "我的名字"; x.NameAuto = false })
	src.Detail.LiveSites[0].Name = "被覆盖的名字"
	st.SyncLiveSourcesForSource(src)
	got, _ := st.GetLiveSource(target.ID)
	if got.Name != "我的名字" {
		t.Fatalf("手动改名不应被同步覆盖: %s", got.Name)
	}

	// 删除（auto → 墓碑）后，父源再同步不复活
	if n := st.DeleteLiveSources([]string{target.ID}); n != 1 {
		t.Fatalf("删除数错误: %d", n)
	}
	st.SyncLiveSourcesForSource(src)
	if got, _ := st.GetLiveSource(target.ID); !got.Hidden {
		t.Fatal("删除的 auto 源应保持墓碑状态")
	}
	list, _ = st.ListLiveSourcesPage(LiveFilter{})
	for _, x := range list {
		if x.URL == "http://live.example/a.m3u" {
			t.Fatal("墓碑不应出现在列表里")
		}
	}

	// 父源里地址消失 → 解除关联，墓碑保持墓碑
	src.Detail.LiveSites = src.Detail.LiveSites[1:] // a.m3u 消失，只剩 b.txt
	st.SyncLiveSourcesForSource(src)
	got2, _ := st.GetLiveSource(target.ID)
	if !got2.Hidden {
		t.Fatal("墓碑应保持墓碑状态")
	}
	if len(got2.ParentIDs) != 0 {
		t.Fatalf("地址消失应解除关联: %v", got2.ParentIDs)
	}

	// 父源被删 → 关联解除；条目本身保留（本例中它之前已被删除、是墓碑，
	// 保留即墓碑原样；未删过的孤儿条目同样保留，由用户在页面上清理）
	if n := st.Delete([]string{src.ID}); n != 1 {
		t.Fatalf("父源删除失败: %d", n)
	}
	got3, ok := st.GetLiveSource(target.ID)
	if !ok {
		t.Fatal("父源删除不应连带删除直播源记录")
	}
	if len(got3.ParentIDs) != 0 {
		t.Fatalf("父源删除后应解除关联: %v", got3.ParentIDs)
	}
}

func TestLiveCheckResultAndFilter(t *testing.T) {
	st := newTestStore(t)
	ids, _ := st.AddLiveSources([]model.LiveSource{
		{Name: "A", URL: "http://a/1.m3u", Enabled: true},
		{Name: "B", URL: "http://b/1.m3u", Enabled: true},
	})
	_ = st.UpdateLiveCheckResult(ids[0], LiveCheckResult{
		Status: model.StatusOK, HTTPCode: 200, LatencyMs: 123,
		ChannelCount: 100, GroupCount: 5, AliveCount: 18, SampleCount: 20,
		Msg: "ok",
	}, true)
	_ = st.UpdateLiveCheckResult(ids[1], LiveCheckResult{
		Status: model.StatusFail, Msg: "上游超时",
	}, true)

	// 失效源不应出现在订阅数据源里
	enabled := st.EnabledLiveSources()
	if len(enabled) != 1 || enabled[0].Name != "A" {
		t.Fatalf("EnabledLiveSources 应只含检测成功的 A: %+v", enabled)
	}
	a := enabled[0]
	if a.ChannelCount != 100 || a.GroupCount != 5 || a.AliveCount != 18 || a.SampleCount != 20 || a.OKStreak != 1 {
		t.Fatalf("检测结果回写错误: %+v", a)
	}
	b, _ := st.GetLiveSource(ids[1])
	if b.FailStreak != 1 || b.Status != model.StatusFail {
		t.Fatalf("失败源字段错误: %+v", b)
	}

	// 筛选：按状态
	list, total := st.ListLiveSourcesPage(LiveFilter{Status: model.StatusFail})
	if total != 1 || list[0].Name != "B" {
		t.Fatalf("状态筛选错误: %d", total)
	}
}

func TestLiveBatchAction(t *testing.T) {
	st := newTestStore(t)
	ids, _ := st.AddLiveSources([]model.LiveSource{
		{Name: "A", URL: "http://a/1.m3u"},
		{Name: "B", URL: "http://b/1.m3u"},
	})
	n, err := st.BatchLiveSources(ids, "disable", "")
	if err != nil || n != 2 {
		t.Fatalf("批量禁用失败: n=%d err=%v", n, err)
	}
	list, _ := st.ListLiveSourcesPage(LiveFilter{Enabled: "false"})
	if len(list) != 2 {
		t.Fatalf("禁用筛选应命中 2 条: %d", len(list))
	}
	if _, err := st.BatchLiveSources(ids, "group", "央视"); err != nil {
		t.Fatal(err)
	}
	list, _ = st.ListLiveSourcesPage(LiveFilter{Group: "央视"})
	if len(list) != 2 {
		t.Fatalf("移组失败: %d", len(list))
	}
}
