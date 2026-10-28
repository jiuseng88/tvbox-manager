package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

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
	src := model.Source{Name: "单仓源", URL: "http://a.example/1.json", Type: model.TypeSingle, Enabled: true}
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
	src := model.Source{Name: "单仓源", URL: "http://b.example/1.json", Type: model.TypeSingle, Enabled: true}
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
	single := model.Source{Name: "单仓", URL: "http://m.example/1.json", Type: model.TypeSingle, Enabled: true}
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
	src := model.Source{Name: "源", URL: "http://o.example/1.json", Type: model.TypeSingle, Enabled: true}
	if err := st.Add(&src); err != nil {
		t.Fatal(err)
	}
	if n := len(st.ListFlats(FlatFilter{})); n != 1 {
		t.Fatalf("应生成 1 条，实际 %d", n)
	}
	// 模拟 replace 导入：整批换掉父源
	if err := st.ReplaceAll([]model.Source{{Name: "新源", URL: "http://n.example/1.json", Type: model.TypeSingle, Enabled: true}}); err != nil {
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
	src := model.Source{Name: "源", URL: "http://s.example/1.json", Type: model.TypeSingle, Enabled: true}
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

// 未定型（Type=unknown）的源不进订阅：此时分不清单仓/多仓，
// 把多仓地址当单仓发出去，客户端会拿到一条解析不了的东西。
// 探活确认类型后，站点源应立即生成。
func TestUnknownSourceNotPublishedUntilTyped(t *testing.T) {
	st := newTestStore(t)
	src := model.Source{Name: "未探活", URL: "http://u.example/all.json", Enabled: true}
	if err := st.Add(&src); err != nil {
		t.Fatal(err)
	}
	if n := len(st.ListFlats(FlatFilter{})); n != 0 {
		t.Fatalf("未定型源不应生成站点源，实际 %d", n)
	}
	if n := len(st.EnabledFlats()); n != 0 {
		t.Fatalf("未定型源不应进订阅输出，实际 %d", n)
	}

	// 探活确认为多仓 → 按子仓平铺
	if err := st.UpdateCheckResult(src.ID, CheckResult{
		Status: model.StatusOK, HTTPCode: 200, Type: model.TypeMulti,
		Detail: &model.Detail{SubURLs: []model.SubURL{
			{Name: "子一", URL: "http://u.example/1.json"},
			{Name: "子二", URL: "http://u.example/2.json"},
		}},
	}, false); err != nil {
		t.Fatal(err)
	}
	if n := len(st.ListFlats(FlatFilter{})); n != 2 {
		t.Fatalf("定型为多仓后应平铺 2 条，实际 %d", n)
	}

	// 再变成单仓：老的 2 条要清掉，只留自身地址
	if err := st.UpdateCheckResult(src.ID, CheckResult{
		Status: model.StatusOK, HTTPCode: 200, Type: model.TypeSingle,
	}, false); err != nil {
		t.Fatal(err)
	}
	flats := st.ListFlats(FlatFilter{})
	if len(flats) != 1 {
		t.Fatalf("改成单仓后应只剩 1 条，实际 %d", len(flats))
	}
	if flats[0].URL != "http://u.example/all.json" {
		t.Errorf("单仓站点源地址错误: %s", flats[0].URL)
	}
}

// 上次进程中途退出留下的「检测中」要归位，否则概览永远显示在检测。
func TestPersistedCheckingResetOnLoad(t *testing.T) {
	dir := t.TempDir()
	st, err := New(dir)
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	src := model.Source{Name: "甲", URL: "http://c.example/1.json", Type: model.TypeSingle, Enabled: true}
	if err := st.Add(&src); err != nil {
		t.Fatal(err)
	}
	id := st.ListFlats(FlatFilter{})[0].ID

	// 模拟「探活到一半进程没了」：checking 已被中途落盘带进 data.json
	if err := st.Update(src.ID, func(sp *model.Source) { sp.Status = model.StatusChecking }); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateFlat(id, func(fp *model.FlatSource) { fp.Status = model.StatusChecking }); err != nil {
		t.Fatal(err)
	}

	st2, err := New(dir)
	if err != nil {
		t.Fatalf("重启 New 失败: %v", err)
	}
	if s, _ := st2.Get(src.ID); s.Status != model.StatusUnknown {
		t.Errorf("父源残留 checking 未归位: %s", s.Status)
	}
	if f, _ := st2.GetFlat(id); f.Status != model.StatusUnknown {
		t.Errorf("站点源残留 checking 未归位: %s", f.Status)
	}
	if st2.Stats("", "").Checking != 0 || st2.Stats("", "").FlatChecking != 0 {
		t.Error("概览不应残留检测中计数")
	}
}

// 已删除（隐藏）的条目不应把分组留在下拉里
func TestFlatGroupsSkipHidden(t *testing.T) {
	st := newTestStore(t)
	a := model.Source{Name: "甲", URL: "http://ga.example/1.json", Type: model.TypeSingle, Group: "甲组", Enabled: true}
	b := model.Source{Name: "乙", URL: "http://gb.example/1.json", Type: model.TypeSingle, Group: "乙组", Enabled: true}
	if err := st.Add(&a); err != nil {
		t.Fatal(err)
	}
	if err := st.Add(&b); err != nil {
		t.Fatal(err)
	}
	if got := st.FlatGroups(); len(got) != 2 {
		t.Fatalf("应有 2 个分组，实际 %v", got)
	}
	// 删掉甲组的站点源
	var victim string
	for _, f := range st.ListFlats(FlatFilter{}) {
		if f.Group == "甲组" {
			victim = f.ID
		}
	}
	if victim == "" {
		t.Fatal("没找到甲组站点源")
	}
	if _, err := st.DeleteFlats([]string{victim}); err != nil {
		t.Fatal(err)
	}
	groups := st.FlatGroups()
	if len(groups) != 1 || groups[0] != "乙组" {
		t.Errorf("隐藏条目的分组不应出现，实际 %v", groups)
	}
}

// 同一条线路的不同写法（这里只差一个尾斜杠）必须合并成一条站点源，
// 否则订阅接口会下发两次同一个地址——实测旧数据里确实存在这种重复。
func TestFlatsMergeURLVariants(t *testing.T) {
	st := newTestStore(t)
	a := model.Source{
		Name: "A仓", URL: "http://d.example/a.json", Type: model.TypeMulti, Enabled: true,
		Detail: &model.Detail{SubURLs: []model.SubURL{{Name: "毒盒", URL: "https://d.example/tv"}}},
	}
	b := model.Source{
		Name: "B仓", URL: "http://d.example/b.json", Type: model.TypeMulti, Enabled: true,
		Detail: &model.Detail{SubURLs: []model.SubURL{{Name: "毒盒线路", URL: "https://d.example/tv/"}}},
	}
	if err := st.Add(&a); err != nil {
		t.Fatal(err)
	}
	if err := st.Add(&b); err != nil {
		t.Fatal(err)
	}

	flats := st.ListFlats(FlatFilter{})
	if len(flats) != 1 {
		t.Fatalf("同一条地址的不同写法应合并为 1 条，实际 %d 条: %+v", len(flats), flats)
	}
	if got := len(flats[0].ParentIDs); got != 2 {
		t.Errorf("应带 2 个父引用，实际 %d: %v", got, flats[0].ParentIDs)
	}
	// 首次出现的名称优先（不再随第二个父源改名）
	if flats[0].Name != "毒盒" {
		t.Errorf("名称应保留首次出现的「毒盒」，实际 %q", flats[0].Name)
	}
	// 订阅口径同样只有 1 条
	if n := len(st.EnabledFlats()); n != 1 {
		t.Errorf("订阅应只有 1 条，实际 %d", n)
	}
}

// 存量数据修复：旧版本把 https://x/tv 与 https://x/tv/ 存成了两条，
// 启动时必须合并（父引用累加、只留一条），订阅不再重复。
func TestLoadMergesDuplicateFlats(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()

	src := model.Source{
		ID: "src_multi", Name: "多仓", URL: "http://d.example/all.json",
		Type: model.TypeMulti, Enabled: true, Status: model.StatusOK,
		Detail: &model.Detail{SubURLs: []model.SubURL{{Name: "毒盒", URL: "https://d.example/tv"}}},
	}
	data := model.Data{
		Version: 1,
		Sources: []model.Source{src},
		Flats: []model.FlatSource{
			{ID: "flat_1", Name: "毒盒", URL: "https://d.example/tv", ParentIDs: []string{"src_multi"},
				Enabled: true, Group: "默认", Status: model.StatusOK, CreatedAt: now, UpdatedAt: now},
			{ID: "flat_2", Name: "毒盒线路", URL: "https://d.example/tv/", ParentIDs: []string{"src_multi"},
				Enabled: true, Group: "默认", Status: model.StatusUnknown, CreatedAt: now, UpdatedAt: now},
		},
	}
	b, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "data.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}

	st, err := New(dir)
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	flats := st.ListFlats(FlatFilter{})
	if len(flats) != 1 {
		t.Fatalf("启动后应合并为 1 条，实际 %d 条: %+v", len(flats), flats)
	}
	if flats[0].ID != "flat_1" {
		t.Errorf("应保留先出现的那条，实际 %q", flats[0].ID)
	}
	if n := len(st.EnabledFlats()); n != 1 {
		t.Errorf("订阅应只有 1 条，实际 %d", n)
	}
	if n := len(st.ListFlats(FlatFilter{Enabled: "true"})); n != 1 {
		t.Errorf("启用口径应只有 1 条，实际 %d", n)
	}

	// 合并结果要落盘：重新打开不应又出现两条
	st2, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(st2.ListFlats(FlatFilter{})); n != 1 {
		t.Errorf("重启后仍是 1 条，实际 %d", n)
	}
}
