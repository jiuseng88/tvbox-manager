package store

import (
	"strings"
	"testing"

	"tvbox-manager/internal/model"
)

// 默认排序「可用优先 → 延迟升序 → 创建时间保底」：
// 后台列表与订阅接口共用这套顺序，这里用父源和站点源各验一遍。
func TestSortByAvailLatencyCreated(t *testing.T) {
	st := newTestStore(t)
	src := model.Source{
		Name: "多仓", URL: "http://s.example/all.json", Type: model.TypeMulti, Enabled: true,
		Detail: &model.Detail{SubURLs: []model.SubURL{
			{Name: "慢线", URL: "http://f.example/slow.json"},
			{Name: "快线", URL: "http://f.example/fast.json"},
			{Name: "挂线", URL: "http://f.example/down.json"},
			{Name: "新线", URL: "http://f.example/new.json"},
		}},
	}
	if err := st.Add(&src); err != nil {
		t.Fatal(err)
	}

	setFlat := func(url, status string, ms int64) {
		for _, f := range st.ListFlats(FlatFilter{}) {
			if f.URL != url {
				continue
			}
			if err := st.UpdateFlatCheckResult(f.ID, CheckResult{Status: status, LatencyMs: ms}, true); err != nil {
				t.Fatal(err)
			}
			return
		}
		t.Fatalf("没找到 %s", url)
	}
	setFlat("http://f.example/slow.json", model.StatusOK, 500)
	setFlat("http://f.example/fast.json", model.StatusOK, 100)
	setFlat("http://f.example/down.json", model.StatusFail, 0)

	flats := st.ListFlats(FlatFilter{Enabled: "true"})
	if len(flats) != 4 {
		t.Fatalf("应有 4 条，实际 %d", len(flats))
	}
	want := []string{"快线", "慢线", "挂线", "新线"} // ok 按延迟 → fail → unknown（未回写，按创建时间）
	for i, w := range want {
		if flats[i].Name != w {
			t.Fatalf("第 %d 位应为 %s，实际 %s（顺序: %v %v %v %v）", i+1, w, flats[i].Name,
				flats[0].Name, flats[1].Name, flats[2].Name, flats[3].Name)
		}
	}

	// 订阅接口输出同序：直接复用订阅层的取数路径
	if sub := st.EnabledFlats(); sub[0].Name != "快线" || sub[len(sub)-1].Name != "新线" {
		t.Errorf("订阅取数顺序应为快线在前、新线在末: %s ... %s", sub[0].Name, sub[len(sub)-1].Name)
	}

	// 父源列表同规则：ok 在前按延迟，非 ok 按创建时间
	a := model.Source{Name: "父-慢", URL: "http://p.example/a.json", Type: model.TypeSingle, Enabled: true, Status: model.StatusOK, LatencyMs: 900}
	b := model.Source{Name: "父-快", URL: "http://p.example/b.json", Type: model.TypeSingle, Enabled: true, Status: model.StatusOK, LatencyMs: 50}
	c := model.Source{Name: "父-挂", URL: "http://p.example/c.json", Type: model.TypeSingle, Enabled: true, Status: model.StatusFail}
	_ = st.Add(&a)
	_ = st.Add(&b)
	_ = st.Add(&c)
	sources := st.List(Filter{Enabled: "true"}) // 多仓父源不产生父源条目，这里只有三条单仓
	var got []string
	for _, s := range sources {
		if strings.HasPrefix(s.Name, "父-") {
			got = append(got, s.Name)
		}
	}
	if len(got) != 3 || got[0] != "父-快" || got[1] != "父-慢" || got[2] != "父-挂" {
		t.Errorf("父源顺序应为 快/慢/挂: %v", got)
	}
}
