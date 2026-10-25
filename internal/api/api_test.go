package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"tvbox-manager/internal/scheduler"
	"tvbox-manager/internal/store"
)

// testEnv 完整路由 + mock 源站
type testEnv struct {
	t     *testing.T
	st    *store.Store
	sch   *scheduler.Scheduler
	api   http.Handler
	token string
	mock  *httptest.Server // 返回合法单仓
	slow  *httptest.Server // 挂起 800ms，用于探活进度测试
}

func newEnv(t *testing.T) *testEnv {
	t.Helper()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := st.Config()
	cfg.CheckIntervalMinutes = 360
	cfg.StartupCheck = false
	sch := scheduler.Start(st, &cfg)

	webFS := fstest.MapFS{"web/index.html": &fstest.MapFile{Data: []byte("<html>ok</html>")}}
	handler := New(st, sch, "test", webFS).Router()

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"spider":"http://x/s.jar","sites":[
			{"key":"a","name":"A站","type":1,"api":"http://x/api.php"},
			{"key":"b","name":"B站","type":1,"api":"http://y/api.php"}],
			"lives":[{"name":"直播1"}],"parses":[{"name":"p1"}]}`))
	}))
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(800 * time.Millisecond)
		_, _ = w.Write([]byte(`{"sites":[]}`))
	}))

	env := &testEnv{t: t, st: st, sch: sch, api: handler, token: st.Config().Token, mock: mock, slow: slow}
	t.Cleanup(func() {
		sch.Shutdown()
		mock.Close()
		slow.Close()
	})
	return env
}

// do 发请求（自动带 token），返回状态码与响应体
func (e *testEnv) do(method, path, token, body string, ct string) (int, string) {
	e.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rd)
	if token != "" {
		req.Header.Set("X-Token", token)
	}
	if ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	rec := httptest.NewRecorder()
	e.api.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func (e *testEnv) doJSON(method, path string, v interface{}) (int, map[string]interface{}) {
	e.t.Helper()
	b, _ := json.Marshal(v)
	code, body := e.do(method, path, e.token, string(b), "application/json")
	var out map[string]interface{}
	_ = json.Unmarshal([]byte(body), &out)
	return code, out
}

func dataOf(t *testing.T, body map[string]interface{}) map[string]interface{} {
	t.Helper()
	d, _ := body["data"].(map[string]interface{})
	if d == nil {
		t.Fatalf("响应缺少 data: %v", body)
	}
	return d
}

// ---------- 鉴权 ----------

func TestAuth(t *testing.T) {
	e := newEnv(t)
	if code, _ := e.do("GET", "/api/sources", "", "", ""); code != 401 {
		t.Fatalf("无 token 应 401: %d", code)
	}
	if code, _ := e.do("GET", "/api/sources", "wrong", "", ""); code != 401 {
		t.Fatalf("错 token 应 401: %d", code)
	}
	// 三种传递方式
	for _, pass := range []func(*http.Request){
		func(r *http.Request) { r.Header.Set("X-Token", e.token) },
		func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+e.token) },
	} {
		req := httptest.NewRequest("GET", "/api/sources", nil)
		pass(req)
		rec := httptest.NewRecorder()
		e.api.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("合法 token 应 200: %d", rec.Code)
		}
	}
	req := httptest.NewRequest("GET", "/api/sources?token="+e.token, nil)
	rec := httptest.NewRecorder()
	e.api.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("query token 应 200: %d", rec.Code)
	}
	// healthz 与 /sub 免鉴权
	if code, _ := e.do("GET", "/healthz", "", "", ""); code != 200 {
		t.Fatal("healthz 应免鉴权")
	}
	if code, _ := e.do("GET", "/sub", "", "", ""); code != 200 {
		t.Fatal("/sub 应免鉴权")
	}
}

// ---------- 源 CRUD + 异步探活 ----------

func TestSourceCRUDAndAutoCheck(t *testing.T) {
	e := newEnv(t)

	// 新增（autoCheck 默认 true，异步探活 mock 源）
	code, body := e.doJSON("POST", "/api/sources", map[string]interface{}{
		"name": "测试源", "url": e.mock.URL + "/config.json", "group": "常用",
	})
	if code != 200 {
		t.Fatalf("新增失败: %d %v", code, body)
	}
	src := dataOf(t, body)
	id := src["id"].(string)
	if src["group"] != "常用" {
		t.Fatalf("分组错误: %v", src["group"])
	}

	// 轮询等异步探活完成（mock 是本地地址，guard=false 不拦）
	deadline := time.Now().Add(5 * time.Second)
	okStatus := false
	for time.Now().Before(deadline) {
		_, body := e.doJSON("GET", "/api/sources/"+id, nil)
		s := dataOf(t, body)
		if s["status"] == "ok" {
			okStatus = true
			d := s["detail"].(map[string]interface{})
			if d["sites"].(float64) != 2 {
				t.Fatalf("站点数错误: %v", d["sites"])
			}
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !okStatus {
		t.Fatal("autoCheck 未在限时内变为 ok")
	}

	// 重复地址拒绝
	code, _ = e.doJSON("POST", "/api/sources", map[string]interface{}{"url": e.mock.URL + "/config.json"})
	if code != 409 {
		t.Fatalf("重复地址应 409: %d", code)
	}

	// 修改
	code, body = e.doJSON("PUT", "/api/sources/"+id, map[string]interface{}{"name": "改名", "sort": 5})
	if code != 200 || dataOf(t, body)["name"] != "改名" {
		t.Fatalf("修改失败: %d %v", code, body)
	}

	// toggle
	code, body = e.doJSON("POST", "/api/sources/"+id+"/toggle", nil)
	if code != 200 || dataOf(t, body)["enabled"] == true {
		t.Fatal("toggle 应翻转为禁用")
	}

	// 列表 + 过滤
	code, body = e.doJSON("GET", "/api/sources?enabled=false", nil)
	if code != 200 {
		t.Fatal("列表失败")
	}
	if dataOf(t, body)["total"].(float64) != 1 {
		t.Fatalf("过滤计数错误: %v", body["data"])
	}

	// 删除
	if code, _ := e.doJSON("DELETE", "/api/sources/"+id, nil); code != 200 {
		t.Fatal("删除失败")
	}
	if code, _ := e.doJSON("GET", "/api/sources/"+id, nil); code != 404 {
		t.Fatal("删除后应 404")
	}
}

func TestBatchAddAndAction(t *testing.T) {
	e := newEnv(t)
	urls := []string{
		"名称甲|" + e.mock.URL + "/a.json",
		"名称乙," + e.mock.URL + "/b.json",
		e.mock.URL + "/c.json",
		e.mock.URL + "/a.json", // 重复
	}
	code, body := e.doJSON("POST", "/api/sources/batch", map[string]interface{}{
		"urls": urls, "group": "批量组",
	})
	if code != 200 {
		t.Fatalf("批量添加失败: %d", code)
	}
	d := dataOf(t, body)
	if d["added"].(float64) != 3 {
		t.Fatalf("added 应为 3: %v", d)
	}
	skipped, _ := d["skipped"].([]interface{})
	if len(skipped) != 1 {
		t.Fatalf("skipped 应为 1: %v", d)
	}

	// onDuplicate=update 更新已有
	code, _ = e.doJSON("POST", "/api/sources/batch", map[string]interface{}{
		"urls": []string{"新名甲|" + e.mock.URL + "/a.json"}, "onDuplicate": "update",
	})
	if code != 200 {
		t.Fatal("update 模式失败")
	}

	// 批量禁用
	list := e.st.List(store.Filter{})
	var ids []string
	for _, s := range list {
		ids = append(ids, s.ID)
	}
	code, body = e.doJSON("POST", "/api/sources/batch-action", map[string]interface{}{
		"ids": ids, "action": "disable",
	})
	if code != 200 || dataOf(t, body)["affected"].(float64) != 3 {
		t.Fatalf("批量禁用失败: %v", body)
	}
	// 名称来自 | / , 分隔
	found := false
	for _, s := range e.st.List(store.Filter{}) {
		if s.Name == "新名甲" {
			found = true
		}
	}
	if !found {
		t.Fatal("onDuplicate=update 未更新名称")
	}
}

// ---------- 文件导入 ----------

func TestImportTxtAndJSON(t *testing.T) {
	e := newEnv(t)

	// .txt：名称,URL / 名称|URL / # 注释混排
	txt := "# 注释行\n肥猫,http://m1.example/1.json\n饭太硬|http://m2.example/2.json\nhttp://m3.example/3.json\n"
	code, body := e.multipart("/api/sources/import", "sources.txt", txt, "")
	if code != 200 {
		t.Fatalf("txt 导入失败: %d %s", code, body)
	}
	var out map[string]interface{}
	_ = json.Unmarshal([]byte(body), &out)
	if out["data"].(map[string]interface{})["added"].(float64) != 3 {
		t.Fatalf("txt 导入条数错误: %s", body)
	}

	// .json urls 形态
	jsonBody := `{"urls":[{"url":"http://m4.example/4.json","name":"甲"},{"url":"http://m5.example/5.json"}]}`
	code, body = e.multipart("/api/sources/import", "list.json", jsonBody, "")
	if code != 200 {
		t.Fatalf("json 导入失败: %d %s", code, body)
	}
	_ = json.Unmarshal([]byte(body), &out)
	if out["data"].(map[string]interface{})["added"].(float64) != 2 {
		t.Fatalf("json 导入条数错误: %s", body)
	}

	// 仓库内容文件 → 明确报错
	siteBody := `{"sites":[{"key":"a","name":"A","type":1,"api":"http://x"}]}`
	code, body = e.multipart("/api/sources/import", "repo.json", siteBody, "")
	if code != 400 || !strings.Contains(body, "仓库内容") {
		t.Fatalf("仓库内容文件应报错: %d %s", code, body)
	}

	// 超 2MB 拒绝
	big := "# x\n" + strings.Repeat("http://big.example/"+strings.Repeat("a", 100)+".json\n", 25000)
	code, _ = e.multipart("/api/sources/import", "big.txt", big, "")
	if code != 400 {
		t.Fatalf("超大文件应 400: %d", code)
	}
}

func (e *testEnv) multipart(path, filename, content, group string) (int, string) {
	e.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", filename)
	_, _ = fw.Write([]byte(content))
	if group != "" {
		_ = mw.WriteField("group", group)
	}
	_ = mw.Close()

	req := httptest.NewRequest("POST", path, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-Token", e.token)
	rec := httptest.NewRecorder()
	e.api.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// ---------- 预览与 SSRF ----------

func TestPreviewAndSSRF(t *testing.T) {
	e := newEnv(t)

	// 入库源 → 预览可用（mock 是本地地址，入库源不做内网拦截）
	code, body := e.doJSON("POST", "/api/sources", map[string]interface{}{
		"url": e.mock.URL + "/config.json", "autoCheck": false,
	})
	id := dataOf(t, body)["id"].(string)
	code, body = e.doJSON("GET", "/api/sources/"+id+"/preview", nil)
	if code != 200 {
		t.Fatalf("入库源预览失败: %d", code)
	}
	pv := dataOf(t, body)
	if pv["type"] != "single" {
		t.Fatalf("预览类型错误: %v", pv["type"])
	}
	det := pv["detail"].(map[string]interface{})
	if det["sites"].(float64) != 2 {
		t.Fatalf("预览站点数错误: %v", det["sites"])
	}
	if _, ok := pv["raw"].(string); !ok {
		t.Fatal("预览应包含原始 JSON")
	}

	// 任意 URL 预览 → SSRF 拦截（httptest 是 127.0.0.1）
	code, body = e.doJSON("GET", "/api/preview?url="+e.mock.URL+"/x.json", nil)
	if code != 200 || !strings.Contains(fmt.Sprint(body), "拒绝") {
		t.Fatalf("SSRF 应被拦截: %d %v", code, body)
	}
	// 元数据地址字面量
	code, _ = e.doJSON("GET", "/api/preview?url=http://169.254.169.254/latest/meta-data/", nil)
	if code != 200 { // 拦截发生在 Fetch 内，响应仍是 200 + data.errMsg
		t.Logf("169.254 预览返回 %d", code)
	}
}

// ---------- 探活调度 ----------

func TestCheckRunAndProgress(t *testing.T) {
	e := newEnv(t)
	// 一个 mock（快） + 一个 slow（800ms），保证检测运行期间发起第二次触发能命中 409
	e.doJSON("POST", "/api/sources", map[string]interface{}{"url": e.mock.URL + "/1.json", "autoCheck": false})
	e.doJSON("POST", "/api/sources", map[string]interface{}{"url": e.slow.URL + "/slow.json", "autoCheck": false})

	code, body := e.doJSON("POST", "/api/check/run", nil)
	if code != 200 {
		t.Fatalf("手动探活失败: %d", code)
	}
	// 立即再触发 → 409
	code, _ = e.doJSON("POST", "/api/check/run", nil)
	if code != 409 {
		t.Fatalf("运行中再触发应 409: %d", code)
	}
	// 轮询进度直到完成
	deadline := time.Now().Add(10 * time.Second)
	done := false
	for time.Now().Before(deadline) {
		_, body := e.doJSON("GET", "/api/check/status", nil)
		p := dataOf(t, body)
		if p["running"] == false && p["done"].(float64) == 2 {
			done = true
			if p["ok"].(float64) != 1 {
				t.Fatalf("ok 计数应 1（slow 返回结构不完整判失败）: %v", p)
			}
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !done {
		t.Fatal("探活未在限时内完成")
	}

	// 结果已回写
	_, body = e.doJSON("GET", "/api/stats", nil)
	st := dataOf(t, body)
	if st["total"].(float64) != 2 || st["ok"].(float64) != 1 {
		t.Fatalf("统计错误: %v", st)
	}
}

// ---------- 订阅输出 ----------

func TestSubOutputs(t *testing.T) {
	e := newEnv(t)
	e.doJSON("POST", "/api/sources", map[string]interface{}{"name": "甲", "url": "http://s1.example/1.json", "autoCheck": false, "sort": 2})
	e.doJSON("POST", "/api/sources", map[string]interface{}{"name": "乙", "url": "http://s2.example/2.json", "autoCheck": false, "sort": 1})
	// 丙：禁用
	code, body := e.doJSON("POST", "/api/sources", map[string]interface{}{"name": "丙", "url": "http://s3.example/3.json", "autoCheck": false})
	if code != 200 {
		t.Fatal("添加丙失败")
	}
	e.doJSON("POST", "/api/sources/"+dataOf(t, body)["id"].(string)+"/toggle", nil)

	// 默认：仅启用、按 sort
	code, bodyStr := e.do("GET", "/sub", "", "", "")
	if code != 200 {
		t.Fatal("/sub 失败")
	}
	var sub map[string]interface{}
	_ = json.Unmarshal([]byte(bodyStr), &sub)
	urls := sub["urls"].([]interface{})
	if len(urls) != 2 {
		t.Fatalf("/sub 应只含 2 条启用源: %s", bodyStr)
	}
	if urls[0].(map[string]interface{})["name"] != "乙" {
		t.Fatal("sort=1 的乙应排第一")
	}

	// name 前缀
	_, bodyStr = e.do("GET", "/sub?name=客厅", "", "", "")
	if !strings.Contains(bodyStr, `"客厅-乙"`) {
		t.Fatalf("前缀输出错误: %s", bodyStr)
	}

	// format=fan
	_, bodyStr = e.do("GET", "/sub?format=fan", "", "", "")
	if !strings.Contains(bodyStr, `"type":0`) {
		t.Fatalf("fan 输出应含 type:0: %s", bodyStr)
	}

	// txt
	_, bodyStr = e.do("GET", "/sub/txt", "", "", "")
	if !strings.Contains(bodyStr, "http://s2.example/2.json") || strings.Contains(bodyStr, "s3.example") {
		t.Fatalf("txt 输出错误: %s", bodyStr)
	}

	// all：需 token，含禁用
	if code, _ := e.do("GET", "/sub/all", "", "", ""); code != 401 {
		t.Fatal("/sub/all 应需 token")
	}
	_, bodyStr = e.do("GET", "/sub/all", e.token, "", "")
	if !strings.Contains(bodyStr, "s3.example") {
		t.Fatalf("/sub/all 应含禁用源: %s", bodyStr)
	}

	// 响应头
	req := httptest.NewRequest("GET", "/sub", nil)
	rec := httptest.NewRecorder()
	e.api.ServeHTTP(rec, req)
	if rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatal("订阅响应头缺失")
	}
}

// ---------- 配置与备份 ----------

func TestConfigAndBackup(t *testing.T) {
	e := newEnv(t)

	// 脱敏
	_, body := e.doJSON("GET", "/api/config", nil)
	cfg := dataOf(t, body)["config"].(map[string]interface{})
	if cfg["token"] != "**" {
		t.Fatalf("Token 应脱敏: %v", cfg["token"])
	}

	// 修改配置
	cfg["checkIntervalMinutes"] = 60
	code, body := e.doJSON("PUT", "/api/config", cfg)
	if code != 200 {
		t.Fatalf("配置修改失败: %d %v", code, body)
	}
	if e.st.Config().CheckIntervalMinutes != 60 {
		t.Fatal("配置未生效")
	}

	// 非法值
	bad := e.st.Config()
	bad.CheckIntervalMinutes = -1
	if code, _ := e.doJSON("PUT", "/api/config", bad); code != 400 {
		t.Fatal("非法间隔应 400")
	}

	// 令牌修改
	if code, _ := e.doJSON("POST", "/api/config/token", map[string]string{"oldToken": "wrong", "newToken": "abc12345"}); code != 401 {
		t.Fatal("旧令牌错误应 401")
	}
	code, _ = e.doJSON("POST", "/api/config/token", map[string]string{"oldToken": e.token, "newToken": "new-token-9"})
	if code != 200 {
		t.Fatal("令牌修改失败")
	}
	e.token = "new-token-9"
	if code, _ := e.do("GET", "/api/sources", e.token, "", ""); code != 200 {
		t.Fatal("新令牌应可用")
	}

	// 导出 → 清空 → 导入 replace
	e.doJSON("POST", "/api/sources", map[string]interface{}{"name": "备份源", "url": "http://bk.example/1.json", "autoCheck": false})
	_, expBody := e.doJSON("GET", "/api/export", nil)
	exp := dataOf(t, expBody)
	if _, ok := exp["sources"]; !ok {
		t.Fatal("导出缺少 sources")
	}
	// token 不导出
	if strings.Contains(fmt.Sprint(exp["config"]), e.token) {
		t.Fatal("导出不应包含 token")
	}

	importPayload := map[string]interface{}{"mode": "replace", "data": exp}
	code, _ = e.doJSON("POST", "/api/import/config", importPayload)
	if code != 200 {
		t.Fatal("导入失败")
	}
	if e.st.Count(store.Filter{}) != 1 {
		t.Fatalf("replace 导入后应 1 条: %d", e.st.Count(store.Filter{}))
	}
}

// ---------- 静态页 ----------

func TestStaticIndex(t *testing.T) {
	e := newEnv(t)
	code, body := e.do("GET", "/", "", "", "")
	if code != 200 || !strings.Contains(body, "ok") {
		t.Fatalf("首页应输出嵌入的 index.html: %d", code)
	}
}
