package store

import (
	"testing"

	"tvbox-manager/internal/model"
)

// 启动对账必须保留站点源的身份与用户改动，不能「清空重建」。
// 这里模拟：改名 + 禁用 + 探活成功 → 重启 → 全部保留。
func TestFlatsSurviveRestart(t *testing.T) {
	dir := t.TempDir()
	st, err := New(dir)
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	src := model.Source{Name: "单仓源", URL: "http://a.example/1.json", Enabled: true}
	if err := st.Add(&src); err != nil {
		t.Fatal(err)
	}
	flats := st.ListFlats(FlatFilter{})
	if len(flats) != 1 {
		t.Fatalf("单仓父源应生成 1 条站点源，实际 %d", len(flats))
	}
	id := flats[0].ID

	// 用户改动：改名 + 禁用 + 探活结果
	if err := st.UpdateFlat(id, func(fp *model.FlatSource) {
		fp.Name = "我改的名字"
		fp.NameAuto = false
		fp.Enabled = false
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateFlatCheckResult(id, CheckResult{Status: model.StatusOK, HTTPCode: 200}, true); err != nil {
		t.Fatal(err)
	}

	// 重启
	st2, err := New(dir)
	if err != nil {
		t.Fatalf("重启 New 失败: %v", err)
	}
	got, found := st2.GetFlat(id)
	if !found {
		t.Fatalf("重启后站点源 ID 变了或丢失（ID 必须稳定）")
	}
	if got.Name != "我改的名字" {
		t.Errorf("改名被同步覆盖: %q", got.Name)
	}
	if got.Enabled {
		t.Error("禁用状态被重启重置为启用")
	}
	if got.Status != model.StatusOK || got.CheckCount != 1 {
		t.Errorf("探活结果丢失: status=%s count=%d", got.Status, got.CheckCount)
	}
	if n := len(st2.ListFlats(FlatFilter{})); n != 1 {
		t.Errorf("重启后站点源条数错误: %d", n)
	}
}

// 删除 = 隐藏 + 落盘；重启后不复活，父源再次同步也不复活。
func TestDeleteFlatHiddenAndPersisted(t *testing.T) {
	dir := t.TempDir()
	st, err := New(dir)
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	src := model.Source{Name: "单仓源", URL: "http://b.example/1.json", Enabled: true}
	if err := st.Add(&src); err != nil {
		t.Fatal(err)
	}
	flats := st.ListFlats(FlatFilter{})
	if len(flats) != 1 {
		t.Fatalf("应生成 1 条站点源，实际 %d", len(flats))
	}
	id := flats[0].ID

	n, err := st.DeleteFlats([]string{id})
	if err != nil {
		t.Fatalf("删除落盘失败: %v", err)
	}
	if n != 1 {
		t.Fatalf("删除数错误: %d", n)
	}
	if len(st.ListFlats(FlatFilter{})) != 0 {
		t.Fatal("删除后列表不应再出现")
	}
	// 重复删除不应重复计数
	if n2, _ := st.DeleteFlats([]string{id}); n2 != 0 {
		t.Errorf("重复删除应计 0，实际 %d", n2)
	}

	// 父源再次同步（改名会触发 syncFlatsForSourceLocked）→ 不复活
	if err := st.Update(src.ID, func(sp *model.Source) { sp.Name = "改个名" }); err != nil {
		t.Fatal(err)
	}
	if n := len(st.ListFlats(FlatFilter{})); n != 0 {
		t.Fatalf("父源同步后已删除条目复活了: %d 条", n)
	}

	// 重启 → 仍然不复活
	st2, err := New(dir)
	if err != nil {
		t.Fatalf("重启 New 失败: %v", err)
	}
	if n := len(st2.ListFlats(FlatFilter{})); n != 0 {
		t.Fatalf("重启后被删条目复活了: %d 条", n)
	}
	// 订阅层也不能再下发
	if n := len(st2.EnabledFlats()); n != 0 {
		t.Errorf("已删除条目仍出现在订阅输出: %d", n)
	}
}

// 多仓父源：子仓平铺、重复地址合并父引用、子仓消失后摘引用。
func TestFlatsFromMultiSource(t *testing.T) {
	st := newTestStore(t)
	src := model.Source{
		Name:    "多仓",
		URL:     "http://m.example/all.json",
		Type:    model.TypeMulti,
		Enabled: true,
		Detail: &model.Detail{SubURLs: []model.SubURL{
			{Name: "子一", URL: "http://m.example/1.json"},
			{Name: "子二", URL: "http://m.example/2.json"},
		}},
	}
	if err := st.Add(&src); err != nil {
		t.Fatal(err)
	}
	if n := len(st.ListFlats(FlatFilter{})); n != 2 {
		t.Fatalf("多仓应平铺出 2 条站点源，实际 %d", n)
	}

	// 同地址的单仓父源并入同一条站点源（多父引用）
	single := model.Source{Name: "单仓", URL: "http://m.example/1.json", Enabled: true}
	if err := st.Add(&single); err != nil {
		t.Fatal(err)
	}
	flats := st.ListFlats(FlatFilter{})
	if len(flats) != 2 {
		t.Fatalf("同地址应合并，实际 %d 条", len(flats))
	}
	var merged model.FlatSource
	for _, f := range flats {
		if f.URL == "http://m.example/1.json" {
			merged = f
		}
	}
	if len(merged.ParentIDs) != 2 {
		t.Errorf("应有 2 个父引用，实际 %v", merged.ParentIDs)
	}

	// 子仓从多仓消失 → 只摘引用，单仓父源还在，条目保留
	if err := st.Update(src.ID, func(sp *model.Source) {
		if sp.Detail != nil {
			sp.Detail.SubURLs = sp.Detail.SubURLs[:1] // 只留子一
		}
	}); err != nil {
		t.Fatal(err)
	}
	if n := len(st.ListFlats(FlatFilter{})); n != 1 {
		t.Fatalf("子二无父引用应被清理，实际 %d 条", n)
	}
}

// 故障注入回归：父源被删光后，对账要清掉孤儿站点源（replace 导入场景）。
func TestResyncDropsOrphanFlats(t *testing.T) {
	st := newTestStore(t)
	src := model.Source{Name: "源", URL: "http://o.example/1.json", Enabled: true}
	if err := st.Add(&src); err != nil {
		t.Fatal(err)
	}
	if n := len(st.ListFlats(FlatFilter{})); n != 1 {
		t.Fatalf("应生成 1 条，实际 %d", n)
	}
	// 模拟 replace 导入：整批换掉父源
	if err := st.ReplaceAll([]model.Source{{Name: "新源", URL: "http://n.example/1.json", Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	flats := st.ListFlats(FlatFilter{})
	if len(flats) != 1 {
		t.Fatalf("replace 后应只剩新源的 1 条，实际 %d", len(flats))
	}
	if flats[0].URL != "http://n.example/1.json" {
		t.Errorf("残留了旧父源的站点源: %s", flats[0].URL)
	}
}

// 站点源探活计数：连续失败次数成功即清零（自动禁用看的是连续，不是累计）
func TestFlatCheckStreakReset(t *testing.T) {
	st := newTestStore(t)
	src := model.Source{Name: "源", URL: "http://s.example/1.json", Enabled: true}
	if err := st.Add(&src); err != nil {
		t.Fatal(err)
	}
	f := st.ListFlats(FlatFilter{})[0]

	for i := 0; i < 2; i++ {
		_ = st.UpdateFlatCheckResult(f.ID, CheckResult{Status: model.StatusFail}, false)
	}
	_ = st.UpdateFlatCheckResult(f.ID, CheckResult{Status: model.StatusOK, HTTPCode: 200}, false)
	got, _ := st.GetFlat(f.ID)
	if got.FailStreak != 0 || got.OKStreak != 1 {
		t.Errorf("成功后连续失败应清零: failStreak=%d okStreak=%d", got.FailStreak, got.OKStreak)
	}
	if got.FailCount != 2 || got.CheckCount != 3 {
		t.Errorf("累计计数不应被清零: failCount=%d checkCount=%d", got.FailCount, got.CheckCount)
	}

	// 再失败一次：streak 从 1 开始，而不是接着累计的 2
	_ = st.UpdateFlatCheckResult(f.ID, CheckResult{Status: model.StatusFail}, false)
	got, _ = st.GetFlat(f.ID)
	if got.FailStreak != 1 {
		t.Errorf("连续失败应从 1 重新计: %d", got.FailStreak)
	}
}
