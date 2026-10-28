package api

import (
	"strings"
	"testing"

	"tvbox-manager/internal/model"
	"tvbox-manager/internal/store"
)

// addSrc 直接塞一条父源（绕过探活），typ 决定会不会生成站点源
func (e *testEnv) addSrc(name, url, typ string) string {
	e.t.Helper()
	src := model.Source{Name: name, URL: url, Type: typ, Enabled: true, Group: "默认"}
	if err := e.st.Add(&src); err != nil {
		e.t.Fatalf("添加源失败: %v", err)
	}
	return src.ID
}

// flatCount 该父源名下可见站点源条数（列表接口回填的 FlatCount）
func (e *testEnv) flatCount(sourceID string) int {
	e.t.Helper()
	for _, s := range e.st.List(store.Filter{}) {
		if s.ID == sourceID {
			return s.FlatCount
		}
	}
	return -1
}

// flatsOf 该父源名下的站点源
func (e *testEnv) flatsOf(sourceID string) []model.FlatSource {
	e.t.Helper()
	var out []model.FlatSource
	for _, f := range e.st.ListFlats(store.FlatFilter{}) {
		for _, pid := range f.ParentIDs {
			if pid == sourceID {
				out = append(out, f)
				break
			}
		}
	}
	return out
}

// TestRestoreRevivesDeletedFlat 「源列表里有、仓源里没有」里最难自查的一类：
// 站点源被误删（置墓碑）后，父源怎么同步都不会复活 —— 恢复接口要能救回来。
func TestRestoreRevivesDeletedFlat(t *testing.T) {
	e := newEnv(t)
	id := e.addSrc("单仓A", "http://a.com/tv.json", model.TypeSingle)

	if n := e.flatCount(id); n != 1 {
		t.Fatalf("单仓源应生成 1 条站点源，得到 %d", n)
	}
	flats := e.flatsOf(id)
	if _, err := e.st.DeleteFlats([]string{flats[0].ID}); err != nil {
		t.Fatalf("删除站点源失败: %v", err)
	}
	if n := e.flatCount(id); n != 0 {
		t.Fatalf("删掉后应剩 0 条，得到 %d", n)
	}

	code, body := e.doJSON("POST", "/api/sources/"+id+"/restore", nil)
	if code != 200 {
		t.Fatalf("恢复接口应 200，得到 %d: %v", code, body)
	}
	d := dataOf(t, body)
	if num(d, "restored") != 1 {
		t.Fatalf("应恢复 1 条，得到 %v", body)
	}
	if num(d, "alive") != 1 {
		t.Fatalf("恢复后应有 1 条可用，得到 %v", body)
	}
	if d["hint"] != nil && d["hint"] != "" {
		t.Fatalf("正常恢复不应带提示: %v", body)
	}
	if n := e.flatCount(id); n != 1 {
		t.Fatalf("恢复后列表应显示 1 条，得到 %d", n)
	}
	// 复活后必须重新启用，否则订阅里仍然看不到
	if got := e.flatsOf(id)[0]; !got.Enabled || got.Hidden {
		t.Fatalf("复活后应启用且未隐藏: %+v", got)
	}
}

// TestRestoreUnknownGivesHint 未定型的源没法恢复，必须说清原因而不是静默 0 条
func TestRestoreUnknownGivesHint(t *testing.T) {
	e := newEnv(t)
	id := e.addSrc("未定型", "http://b.com/tv.json", model.TypeUnknown)

	code, body := e.doJSON("POST", "/api/sources/"+id+"/restore", nil)
	if code != 200 {
		t.Fatalf("应 200，得到 %d: %v", code, body)
	}
	d := dataOf(t, body)
	if num(d, "alive") != 0 {
		t.Fatalf("未定型源不该有站点源: %v", body)
	}
	if hint, _ := d["hint"].(string); hint == "" {
		t.Fatalf("未定型源必须返回原因提示: %v", body)
	}
}

// TestRestoreMultiWithoutSubsHint 多仓解析不到子仓地址同样要给出可操作的提示
func TestRestoreMultiWithoutSubsHint(t *testing.T) {
	e := newEnv(t)
	id := e.addSrc("多仓空", "http://c.com/tv.json", model.TypeMulti)

	_, body := e.doJSON("POST", "/api/sources/"+id+"/restore", nil)
	d := dataOf(t, body)
	if hint, _ := d["hint"].(string); hint == "" {
		t.Fatalf("多仓无子仓必须返回原因提示: %v", body)
	}
}

// TestSourceUpdateForceType 手动定型：未定型的源强制单仓后应立即生成站点源、进入订阅
func TestSourceUpdateForceType(t *testing.T) {
	e := newEnv(t)
	id := e.addSrc("待定型", "http://d.com/tv.json", model.TypeUnknown)
	if n := e.flatCount(id); n != 0 {
		t.Fatalf("未定型源不该有站点源，得到 %d", n)
	}

	code, body := e.doJSON("PUT", "/api/sources/"+id, map[string]interface{}{"forceType": "single"})
	if code != 200 {
		t.Fatalf("改类型应 200，得到 %d: %v", code, body)
	}
	src, found := e.st.Get(id)
	if !found {
		t.Fatal("源不见了")
	}
	if src.Type != model.TypeSingle || src.ForceType != model.TypeSingle {
		t.Fatalf("应为强制单仓，得到 type=%s forceType=%s", src.Type, src.ForceType)
	}
	if n := e.flatCount(id); n != 1 {
		t.Fatalf("强制单仓后应立即有 1 条站点源，得到 %d", n)
	}

	// 取消强制定型 → 回到未定型，站点源随之摘掉
	if code, body := e.doJSON("PUT", "/api/sources/"+id, map[string]interface{}{"forceType": ""}); code != 200 {
		t.Fatalf("取消定型应 200，得到 %d: %v", code, body)
	}
	src, _ = e.st.Get(id)
	if src.ForceType != "" || src.Type != model.TypeUnknown {
		t.Fatalf("取消后应回到未定型，得到 type=%s forceType=%s", src.Type, src.ForceType)
	}
	if n := e.flatCount(id); n != 0 {
		t.Fatalf("取消定型后站点源应摘掉，得到 %d", n)
	}
}

// TestForceTypeSurvivesURLEdit 改地址不清手定义的 ForceType
func TestForceTypeSurvivesURLEdit(t *testing.T) {
	e := newEnv(t)
	id := e.addSrc("强制单仓", "http://e.com/old.json", model.TypeSingle)
	if code, body := e.doJSON("PUT", "/api/sources/"+id, map[string]interface{}{"forceType": "single"}); code != 200 {
		t.Fatalf("设置定型失败: %d %v", code, body)
	}
	if code, body := e.doJSON("PUT", "/api/sources/"+id, map[string]interface{}{"url": "http://e.com/new.json"}); code != 200 {
		t.Fatalf("改地址失败: %d %v", code, body)
	}
	src, _ := e.st.Get(id)
	if src.ForceType != model.TypeSingle || src.Type != model.TypeSingle {
		t.Fatalf("改地址不应清掉手动定型，得到 type=%s forceType=%s", src.Type, src.ForceType)
	}
	if n := e.flatCount(id); n != 1 {
		t.Fatalf("改地址后应仍有 1 条站点源，得到 %d", n)
	}
	if got := e.flatsOf(id); len(got) != 1 || got[0].URL != "http://e.com/new.json" {
		t.Fatalf("站点源应跟到新地址: %+v", got)
	}
}

// TestStatsNotInSub 概览上的「未进订阅」口径：启用且零可用站点源
func TestStatsNotInSub(t *testing.T) {
	e := newEnv(t)
	e.addSrc("有仓源", "http://f.com/tv.json", model.TypeSingle)
	e.addSrc("未定型", "http://g.com/tv.json", model.TypeUnknown)
	e.addSrc("多仓空", "http://h.com/tv.json", model.TypeMulti)
	disabled := e.addSrc("禁用", "http://i.com/tv.json", model.TypeSingle)
	if err := e.st.Update(disabled, func(sp *model.Source) { sp.Enabled = false }); err != nil {
		t.Fatal(err)
	}

	_, body := e.doJSON("GET", "/api/stats", nil)
	d := dataOf(t, body)
	if num(d, "notInSub") != 2 {
		t.Fatalf("未进订阅应为 2 条（未定型 + 多仓空），得到 %v", body)
	}
	if num(d, "notInSubUnknown") != 1 || num(d, "notInSubEmpty") != 1 {
		t.Fatalf("原因拆分不对: %v", body)
	}
}

// TestListNotInSubFilter 概览卡片点进来只看到那批源
func TestListNotInSubFilter(t *testing.T) {
	e := newEnv(t)
	good := e.addSrc("有仓源", "http://j.com/tv.json", model.TypeSingle)
	bad := e.addSrc("未定型", "http://k.com/tv.json", model.TypeUnknown)

	_, body := e.doJSON("GET", "/api/sources?notInSub=true&size=200", nil)
	d := dataOf(t, body)
	list, _ := d["list"].([]interface{})
	if len(list) != 1 {
		t.Fatalf("应只剩 1 条未进订阅的源，得到 %d: %v", len(list), body)
	}
	got, _ := list[0].(map[string]interface{})
	if got["id"] != bad {
		t.Fatalf("筛错了源: %v（期望 %s，不应是 %s）", got["id"], bad, good)
	}
	if num(got, "flatCount") != 0 {
		t.Fatalf("未进订阅的源 flatCount 应为 0: %v", got)
	}
}

// TestRestoreThenSubscribe 端到端：恢复后订阅里必须真的出现这条地址
func TestRestoreThenSubscribe(t *testing.T) {
	e := newEnv(t)
	const u = "http://l.com/tv.json"
	id := e.addSrc("单仓", u, model.TypeSingle)
	flats := e.flatsOf(id)
	if _, err := e.st.DeleteFlats([]string{flats[0].ID}); err != nil {
		t.Fatal(err)
	}
	if _, body := e.do("GET", "/sub", "", "", ""); strings.Contains(body, u) {
		t.Fatalf("删除后订阅不该有该地址: %s", body)
	}
	if code, body := e.doJSON("POST", "/api/sources/"+id+"/restore", nil); code != 200 {
		t.Fatalf("恢复失败: %v", body)
	}
	if _, body := e.do("GET", "/sub", "", "", ""); !strings.Contains(body, u) {
		t.Fatalf("恢复后订阅应重新包含该地址: %s", body)
	}
}
