package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tvbox-manager/internal/model"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	return st
}

func TestAddGetDelete(t *testing.T) {
	st := newTestStore(t)
	src := model.Source{Name: "肥猫", URL: "http://example.com/1.json", Group: "常用", Enabled: true}
	if err := st.Add(&src); err != nil {
		t.Fatal(err)
	}
	got, ok := st.Get(src.ID)
	if !ok || got.Name != "肥猫" || got.Group != "常用" {
		t.Fatalf("Get 结果错误: %+v", got)
	}
	if got.CreatedAt.IsZero() {
		t.Error("CreatedAt 应自动填充")
	}
	if n := st.Delete([]string{src.ID}); n != 1 {
		t.Fatalf("删除数错误: %d", n)
	}
	if _, ok := st.Get(src.ID); ok {
		t.Error("删除后不应再查到")
	}
}

func TestListFilterAndSort(t *testing.T) {
	st := newTestStore(t)
	_ = st.Add(&model.Source{Name: "B源", URL: "http://b/2.json", Group: "常用", Enabled: true, Sort: 10, Status: model.StatusOK})
	_ = st.Add(&model.Source{Name: "A源", URL: "http://a/1.json", Group: "备用", Enabled: false, Sort: 10, Status: model.StatusFail})
	_ = st.Add(&model.Source{Name: "C源", URL: "http://c/3.json", Group: "常用", Enabled: true, Sort: 1})

	// enabled 过滤
	enabled := st.List(Filter{Enabled: "true"})
	if len(enabled) != 2 {
		t.Fatalf("enabled 过滤错误: %d", len(enabled))
	}
	// sort 升序、同名按 name
	all := st.List(Filter{})
	if all[0].Name != "C源" || all[1].Name != "A源" || all[2].Name != "B源" {
		t.Fatalf("排序错误: %v %v %v", all[0].Name, all[1].Name, all[2].Name)
	}
	// failFirst：失效置顶
	failFirst := st.List(Filter{Sort: "failFirst"})
	if failFirst[0].Status != model.StatusFail {
		t.Fatalf("failFirst 失效应置顶: %s", failFirst[0].Status)
	}
	// keyword
	kw := st.List(Filter{Keyword: "b/2"})
	if len(kw) != 1 {
		t.Fatalf("keyword 过滤错误: %d", len(kw))
	}
	// 分页
	page := st.List(Filter{Page: 2, Size: 2})
	if len(page) != 1 {
		t.Fatalf("分页错误: %d", len(page))
	}
}

func TestEnabledSourcesCopy(t *testing.T) {
	st := newTestStore(t)
	_ = st.Add(&model.Source{Name: "A", URL: "http://a/1.json", Enabled: true})
	list1 := st.EnabledSources()
	list2 := st.EnabledSources()
	if len(list1) == 0 {
		t.Fatal("应至少 1 条")
	}
	list1[0].Name = "篡改"
	if list2[0].Name == "篡改" {
		t.Fatal("List 返回的必须是副本，不能共享底层数据")
	}
}

func TestUpdateCheckResultStreaks(t *testing.T) {
	st := newTestStore(t)
	src := model.Source{Name: "A", URL: "http://a/1.json", Enabled: true}
	_ = st.Add(&src)

	fail := CheckResult{Status: model.StatusFail, Msg: "HTTP 500", HTTPCode: 500}
	if err := st.UpdateCheckResult(src.ID, fail, true); err != nil {
		t.Fatal(err)
	}
	got, _ := st.Get(src.ID)
	if got.FailStreak != 1 || got.OKStreak != 0 {
		t.Fatalf("连败计数错误: %+v", got)
	}
	okRes := CheckResult{Status: model.StatusOK, Msg: "ok", Detail: &model.Detail{Sites: 5}}
	_ = st.UpdateCheckResult(src.ID, okRes, true)
	got, _ = st.Get(src.ID)
	if got.OKStreak != 1 || got.FailStreak != 0 || got.Detail.Sites != 5 {
		t.Fatalf("成功回写错误: %+v", got)
	}
	if got.LastOKAt == nil || got.LastCheckAt == nil {
		t.Fatal("LastOKAt/LastCheckAt 应填充")
	}
}

func TestCorruptDataBackup(t *testing.T) {
	dir := t.TempDir()
	dataFile := filepath.Join(dir, "data.json")
	if err := os.WriteFile(dataFile, []byte(`{"sources":[broken`), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := New(dir)
	if err != nil {
		t.Fatalf("损坏数据不应导致启动失败: %v", err)
	}
	if len(st.AllSources()) != 0 {
		t.Fatal("损坏后应从空数据开始")
	}
	// 应生成 .corrupt. 备份
	entries, _ := os.ReadDir(dir)
	found := false
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "data.json.corrupt.") {
			found = true
		}
	}
	if !found {
		t.Fatal("应生成 .corrupt. 时间戳备份")
	}
}

// 环境变量只在首次启动（config.json 尚无该项时）提供初始值，
// 落盘后一律以配置文件为准；ENV_OVERRIDE=1 可强制覆盖。
func TestConfigEnvBootstrapThenFileWins(t *testing.T) {
	t.Setenv("TOKEN", "env-token-123")
	t.Setenv("PORT", "9090")
	t.Setenv("SUB_PATH", "/envsub")
	dir := t.TempDir()

	// 首次启动：环境变量作为初始值
	st, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := st.Config()
	if cfg.Token != "env-token-123" || cfg.Port != 9090 || cfg.SubPath != "/envsub" {
		t.Fatalf("首次启动应使用环境变量初值: %+v", cfg)
	}
	if _, err := os.Stat(filepath.Join(dir, "config.json")); err != nil {
		t.Fatal("首次启动应生成 config.json")
	}

	// 后台「设置」页修改：不应再被环境变量压回
	cfg.Port = 8081
	cfg.SubPath = "/mysub"
	cfg.CheckIntervalMinutes = 999
	if err := st.UpdateConfig(cfg); err != nil {
		t.Fatal(err)
	}
	got := st.Config()
	if got.Port != 8081 || got.SubPath != "/mysub" || got.CheckIntervalMinutes != 999 {
		t.Fatalf("后台修改应生效，不被环境变量覆盖: %+v", got)
	}
	// 令牌同样可以在后台改（现在统一走 UpdateConfig）
	cfg.Token = "web-token-9"
	if err := st.UpdateConfig(cfg); err != nil {
		t.Fatal(err)
	}

	// 重启：仍以配置文件为准（端口/订阅路径/令牌都不回到环境变量值）
	st2, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	c2 := st2.Config()
	if c2.Port != 8081 || c2.SubPath != "/mysub" || c2.Token != "web-token-9" {
		t.Fatalf("重启后应以配置文件为准: %+v", c2)
	}

	// ENV_OVERRIDE=1：强制环境变量覆盖配置文件
	t.Setenv("ENV_OVERRIDE", "1")
	st3, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	c3 := st3.Config()
	if c3.Port != 9090 || c3.SubPath != "/envsub" || c3.Token != "env-token-123" {
		t.Fatalf("ENV_OVERRIDE=1 应强制覆盖: %+v", c3)
	}
}

// 手改 config.json 的非法值应被兜底修正
func TestConfigSanitize(t *testing.T) {
	dir := t.TempDir()
	bad := `{"token":"t","port":0,"subPath":"sub/","checkTimeoutSeconds":0,"checkConcurrency":0,"checkIntervalMinutes":-5}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	c := st.Config()
	if c.Port != 8080 || c.SubPath != "/sub" || c.CheckTimeoutSeconds != 15 ||
		c.CheckConcurrency != 10 || c.CheckIntervalMinutes != 360 {
		t.Fatalf("非法配置应被修正: %+v", c)
	}
}

func TestConfigPersistAndTokenMask(t *testing.T) {
	dir := t.TempDir()
	st1, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	token := st1.Config().Token
	if token == "" {
		t.Fatal("首启应生成随机 Token")
	}
	// 写一条数据，触发 data.json 落盘
	_ = st1.Add(&model.Source{Name: "A", URL: "http://a/1.json"})
	// 重开应保持同一 token 与数据
	st2, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if st2.Config().Token != token {
		t.Fatal("Token 应持久化")
	}
	if len(st2.AllSources()) != 1 {
		t.Fatal("数据应持久化")
	}
	// data.json 内容是合法 JSON
	b, err := os.ReadFile(filepath.Join(dir, "data.json"))
	if err != nil {
		t.Fatal(err)
	}
	var d model.Data
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatalf("data.json 应始终合法: %v", err)
	}
}

// 令牌为空（缺失或被手动清空）时必须自动生成并打印：
// 首启横幅「访问令牌」一行展示的就是这个值，开箱即用的保证。
func TestTokenAutoGeneratedWhenEmpty(t *testing.T) {
	dir := t.TempDir()

	// 场景 1：完全没有 config.json（首次启动）
	st, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if tok := st.Config().Token; len(tok) != 16 {
		t.Fatalf("缺失时应生成 16 位随机令牌，实际 %q", tok)
	}

	// 场景 2：config.json 存在但 token 是空串（例如手工编辑时不小心清掉了）
	cfgFile := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgFile, []byte(`{"port":8080,"token":""}`), 0o644); err != nil {
		t.Fatal(err)
	}
	st2, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	tok2 := st2.Config().Token
	if len(tok2) != 16 {
		t.Fatalf("token 为空时应重新生成 16 位随机令牌，实际 %q", tok2)
	}

	// 重新生成的令牌立即落盘，重启后保持不变
	st3, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if st3.Config().Token != tok2 {
		t.Fatalf("重新生成的令牌应落盘: %q != %q", st3.Config().Token, tok2)
	}
}

func TestBatchAction(t *testing.T) {
	st := newTestStore(t)
	var ids []string
	for _, n := range []string{"a", "b", "c"} {
		src := model.Source{Name: n, URL: "http://x/" + n + ".json", Enabled: true}
		_ = st.Add(&src)
		ids = append(ids, src.ID)
	}
	if n, _ := st.BatchAction(ids[:2], "disable", ""); n != 2 {
		t.Fatal("批量禁用失败")
	}
	if len(st.List(Filter{Enabled: "false"})) != 2 {
		t.Fatal("禁用后过滤错误")
	}
	if n, _ := st.BatchAction(ids, "group", "新组"); n != 3 {
		t.Fatal("批量改组失败")
	}
	if len(st.Groups()) != 1 || st.Groups()[0] != "新组" {
		t.Fatal("分组错误")
	}
	if n, _ := st.BatchAction(ids, "delete", ""); n != 3 {
		t.Fatal("批量删除失败")
	}
}

func TestMergeAll(t *testing.T) {
	st := newTestStore(t)
	_ = st.Add(&model.Source{Name: "A", URL: "http://a/1.json"})
	added, skipped := st.MergeAll([]model.Source{
		{Name: "A2", URL: "http://a/1.json"},               // 重复跳过
		{Name: "B", URL: "http://b/2.json", Enabled: true}, // 新增
	})
	if added != 1 || skipped != 1 {
		t.Fatalf("merge 计数错误: added=%d skipped=%d", added, skipped)
	}
}
