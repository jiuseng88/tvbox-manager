package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"tvbox-manager/internal/model"
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
	// 不用 t.TempDir()：导入/批量操作会起后台检测协程，测试结束时可能仍在写 data.json，
	// Windows 上 RemoveAll 会因此报 "The directory is not empty" 把测试判失败
	// （只在 `go test ./...` 并发跑多个包时明显）。这里自己建目录、清理失败就留给系统临时目录。
	dir, err := os.MkdirTemp("", "tvbox-api-test-*")
	if err != nil {
		t.Fatal(err)
	}
	pastedDir, err := os.MkdirTemp("", "tvbox-api-pasted-*")
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := st.Config()
	cfg.CheckIntervalMinutes = 360
	cfg.StartupCheck = false
	cfg.Token = "test-token" // 显式设置令牌；无令牌（无鉴权模式）单独在 TestNoAuthWhenTokenEmpty 覆盖
	if err := st.UpdateConfig(cfg); err != nil {
		t.Fatal(err)
	}
	sch := scheduler.Start(st, &cfg)

	// webFS 以 web 目录为根（生产为 embed 子目录，开发模式为 os.DirFS）
	webFS := fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("<html>ok</html>")}}
	api := New(st, sch, "test", webFS)
	api.PastedDir = pastedDir // 粘贴单仓托管目录
	handler := api.Router()

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
		// 等还在跑的探活收尾，再删临时目录
		for i := 0; i < 100 && sch.Running(); i++ {
			time.Sleep(20 * time.Millisecond)
		}
		mock.Close()
		slow.Close()
		// 导入后的异步检测协程可能仍在落盘：重试删除，实在删不掉就留给系统临时目录，
		// 不能让清理报错把本来通过的用例判失败
		for _, d := range []string{dir, pastedDir} {
			for i := 0; i < 10; i++ {
				if os.RemoveAll(d) == nil {
					break
				}
				time.Sleep(50 * time.Millisecond)
			}
		}
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

// num 取 JSON 数字字段，缺失时返回 -1（便于失败信息直接看到整个响应）
func num(m map[string]interface{}, key string) int {
	v, ok := m[key].(float64)
	if !ok {
		return -1
	}
	return int(v)
}

// markType 把源标记为「已探活确认」的类型。
// 站点源只由已确认类型的源生成（未知类型的源不下发），测试里用它可以跳过真实网络探活。
func (e *testEnv) markType(id, typ string) {
	e.t.Helper()
	if err := e.st.Update(id, func(sp *model.Source) { sp.Type = typ }); err != nil {
		e.t.Fatalf("标记源类型失败: %v", err)
	}
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

	// 仓库内容文件 → 识别为单仓并本服务托管（与粘贴行为一致）
	siteBody := `{"sites":[{"key":"a","name":"A","type":1,"api":"http://x"}]}`
	code, body = e.multipart("/api/sources/import", "repo.json", siteBody, "")
	if code != 200 || !strings.Contains(body, "repo-single") {
		t.Fatalf("仓库内容文件应托管导入: %d %s", code, body)
	}
	_ = json.Unmarshal([]byte(body), &out)
	if out["data"].(map[string]interface{})["added"].(float64) != 1 {
		t.Fatalf("托管导入应新增 1 条: %s", body)
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
	// 轮询进度直到完成。
	// 说明：站点源只由「已确认类型」的源生成，跑之前两个源都是未知类型 → 本轮 total 只有 2 个父源；
	// mock 探活成功后才会生成它的站点源（下一轮才会被当成站点源独立探活）。
	deadline := time.Now().Add(10 * time.Second)
	var p map[string]interface{}
	finished := false
	for time.Now().Before(deadline) {
		_, body := e.doJSON("GET", "/api/check/status", nil)
		p = dataOf(t, body)
		total, _ := p["total"].(float64)
		done, _ := p["done"].(float64)
		if p["running"] == false && total > 0 && done == total {
			finished = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !finished {
		t.Fatalf("探活未在限时内完成，最后进度: %v", p)
	}
	if p["total"].(float64) != 2 || p["ok"].(float64) != 1 || p["fail"].(float64) != 1 {
		t.Fatalf("进度计数错误: %v", p)
	}

	// 结果已回写：父源统计不变，站点源口径单独上报
	_, body = e.doJSON("GET", "/api/stats", nil)
	st := dataOf(t, body)
	if st["total"].(float64) != 2 || st["ok"].(float64) != 1 {
		t.Fatalf("统计错误: %v", st)
	}
	// mock 探活成功 → 生成 1 条站点源（本轮不重复探活，状态还是 unknown）
	if num(st, "flatTotal") != 1 || num(st, "flatUnknown") != 1 {
		t.Fatalf("站点源统计错误: %v", st)
	}
}

// 未知类型的源不下发：多仓地址一旦被当成单仓站点源发出去，客户端那条就会报错
func TestUnknownSourceNotPublished(t *testing.T) {
	e := newEnv(t)
	_, body := e.doJSON("POST", "/api/sources", map[string]interface{}{
		"name": "没探活过", "url": "http://unknown.example/a.json", "autoCheck": false,
	})
	id := dataOf(t, body)["id"].(string)

	_, fb := e.doJSON("GET", "/api/flats?size=50", nil)
	if n := len(dataOf(t, fb)["list"].([]interface{})); n != 0 {
		t.Fatalf("未知类型的源不应生成站点源，实际 %d 条", n)
	}
	_, s := e.do("GET", "/sub", "", "", "")
	if strings.Contains(s, "unknown.example") {
		t.Fatalf("未知类型的源不应进订阅: %s", s)
	}

	// 探活确认是单仓后立即可下发
	e.markType(id, model.TypeSingle)
	_, s = e.do("GET", "/sub", "", "", "")
	if !strings.Contains(s, "unknown.example") {
		t.Fatalf("确认单仓后应进订阅: %s", s)
	}

	// 再从单仓改回未知（等价于改地址后待确认）→ 又不下发
	e.doJSON("PUT", "/api/sources/"+id, map[string]interface{}{"url": "http://unknown2.example/b.json"})
	_, s = e.do("GET", "/sub", "", "", "")
	if strings.Contains(s, "unknown") {
		t.Fatalf("改地址后未确认前不应下发: %s", s)
	}
}

// ---------- 订阅输出 ----------

func TestSubOutputs(t *testing.T) {
	e := newEnv(t)
	// 站点源只由「已确认类型」的源生成，这里用 markType 模拟探活成功回写
	_, b1 := e.doJSON("POST", "/api/sources", map[string]interface{}{"name": "甲", "url": "http://s1.example/1.json", "autoCheck": false, "sort": 2})
	e.markType(dataOf(t, b1)["id"].(string), model.TypeSingle)
	_, b2 := e.doJSON("POST", "/api/sources", map[string]interface{}{"name": "乙", "url": "http://s2.example/2.json", "autoCheck": false, "sort": 1})
	e.markType(dataOf(t, b2)["id"].(string), model.TypeSingle)
	// 丙：禁用
	code, body := e.doJSON("POST", "/api/sources", map[string]interface{}{"name": "丙", "url": "http://s3.example/3.json", "autoCheck": false})
	if code != 200 {
		t.Fatal("添加丙失败")
	}
	idC := dataOf(t, body)["id"].(string)
	e.markType(idC, model.TypeSingle)
	e.doJSON("POST", "/api/sources/"+idC+"/toggle", nil)

	// 给乙回写探活成功：默认排序「可用优先 → 延迟升序 → 创建时间保底」，
	// 乙（ok）应排在未探活的甲前面。
	for _, f := range e.st.ListFlats(store.FlatFilter{}) {
		if f.URL == "http://s2.example/2.json" {
			if err := e.st.UpdateFlatCheckResult(f.ID, store.CheckResult{Status: model.StatusOK, LatencyMs: 88}, true); err != nil {
				t.Fatal(err)
			}
		}
	}
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
		t.Fatal("探活成功的乙应排第一")
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

	// txt：默认仍是纯 URL 每行一条（老客户端不受影响，不能多出逗号）
	_, bodyStr = e.do("GET", "/sub/txt", "", "", "")
	if !strings.Contains(bodyStr, "http://s2.example/2.json") || strings.Contains(bodyStr, "s3.example") {
		t.Fatalf("txt 输出错误: %s", bodyStr)
	}
	if strings.Contains(bodyStr, ",") {
		t.Fatalf("不带 name 时不应输出名称列: %s", bodyStr)
	}

	// txt + name：输出「名称,URL」，支持前缀（与 /sub 的 name 语义一致）
	_, bodyStr = e.do("GET", "/sub/txt?name=客厅", "", "", "")
	if !strings.Contains(bodyStr, "客厅-乙,http://s2.example/2.json") {
		t.Fatalf("txt 带 name 前缀输出错误: %s", bodyStr)
	}
	_, bodyStr = e.do("GET", "/sub/txt?name=", "", "", "")
	if !strings.Contains(bodyStr, "乙,http://s2.example/2.json") {
		t.Fatalf("txt 带空 name 应输出名称但不加前缀: %s", bodyStr)
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

// ---------- 订阅平铺（站点源：多仓展开 + 单仓并入） ----------

func TestSubFlattenMulti(t *testing.T) {
	e := newEnv(t)

	// 源A：多仓，带子仓缓存（Update 触发同步 → 生成站点源）
	code, body := e.doJSON("POST", "/api/sources", map[string]interface{}{
		"name": "聚合A", "url": "http://multi.example/a.json", "autoCheck": false,
	})
	if code != 200 {
		t.Fatalf("添加多仓源失败: %v", body)
	}
	idA := dataOf(t, body)["id"].(string)
	e.st.Update(idA, func(sp *model.Source) {
		sp.Type = model.TypeMulti
		sp.Detail = &model.Detail{SubURLs: []model.SubURL{
			{Name: "甲仓", URL: "http://flat1.example/a.json"},
			{URL: "http://flat2.example/b.json"},
		}}
	})

	// 源B：多仓，flat1 与 A 重复（同 URL 共享同一条站点源，父引用合并）
	code, body = e.doJSON("POST", "/api/sources", map[string]interface{}{
		"name": "聚合B", "url": "http://multi.example/other.json", "autoCheck": false,
	})
	if code != 200 {
		t.Fatalf("添加源B失败: %v", body)
	}
	idB := dataOf(t, body)["id"].(string)
	e.st.Update(idB, func(sp *model.Source) {
		sp.Type = model.TypeMulti
		sp.Detail = &model.Detail{SubURLs: []model.SubURL{
			{Name: "缓存子仓", URL: "http://flat1.example/a.json"}, // 重复 → 首次名称优先
			{URL: "http://flat3.example/c.json"},
		}}
	})

	// 源C：单仓，自身并入
	code, body = e.doJSON("POST", "/api/sources", map[string]interface{}{
		"name": "单仓C", "url": "http://flat4.example/d.json", "autoCheck": false,
	})
	if code != 200 {
		t.Fatalf("添加单仓源失败: %v", body)
	}
	e.markType(dataOf(t, body)["id"].(string), model.TypeSingle)

	_, bodyStr := e.do("GET", "/sub", "", "", "")
	var sub struct {
		URLs []struct {
			URL  string `json:"url"`
			Name string `json:"name"`
		} `json:"urls"`
	}
	if err := json.Unmarshal([]byte(bodyStr), &sub); err != nil {
		t.Fatalf("订阅 JSON 解析失败: %v %s", err, bodyStr)
	}

	// 期望（默认排序：都未探活，同组内按创建时间保底——
	// flat1 甲仓最早，依次是聚合A、聚合B，单仓C 最后添加）：
	want := []struct{ url, name string }{
		{"http://flat1.example/a.json", "甲仓"},
		{"http://flat2.example/b.json", "聚合A"},
		{"http://flat3.example/c.json", "聚合B"},
		{"http://flat4.example/d.json", "单仓C"},
	}
	if len(sub.URLs) != len(want) {
		t.Fatalf("平铺后应 %d 条，实际 %d 条: %s", len(want), len(sub.URLs), bodyStr)
	}
	for i, w := range want {
		if sub.URLs[i].URL != w.url || sub.URLs[i].Name != w.name {
			t.Fatalf("第 %d 条应为 %s(%s)，实际 %s(%s): %s", i+1, w.url, w.name, sub.URLs[i].URL, sub.URLs[i].Name, bodyStr)
		}
	}
	// 多仓地址本身不应出现在订阅结果里
	if strings.Contains(bodyStr, "multi.example") {
		t.Fatalf("订阅输出不应包含多仓地址本身: %s", bodyStr)
	}

	// txt 同样来自站点源
	_, bodyStr = e.do("GET", "/sub/txt", "", "", "")
	if !strings.Contains(bodyStr, "http://flat1.example/a.json") || strings.Contains(bodyStr, "multi.example") {
		t.Fatalf("txt 输出应来自站点源: %s", bodyStr)
	}
}

// ---------- 站点源联动：父源禁用同步、单独禁用互不干扰 ----------

func TestFlatDisableSync(t *testing.T) {
	e := newEnv(t)

	// 多仓源 + 两个子仓
	code, body := e.doJSON("POST", "/api/sources", map[string]interface{}{
		"name": "聚合A", "url": "http://multi.example/a.json", "autoCheck": false,
	})
	if code != 200 {
		t.Fatalf("添加多仓源失败: %v", body)
	}
	idA := dataOf(t, body)["id"].(string)
	e.st.Update(idA, func(sp *model.Source) {
		sp.Type = model.TypeMulti
		sp.Detail = &model.Detail{SubURLs: []model.SubURL{
			{Name: "甲仓", URL: "http://flat1.example/a.json"},
			{URL: "http://flat2.example/b.json"},
		}}
	})

	subContains := func(needle string) bool {
		_, s := e.do("GET", "/sub", "", "", "")
		return strings.Contains(s, needle)
	}
	if !subContains("flat1.example") {
		t.Fatal("初始订阅应含 flat1")
	}

	// 拿到站点源 id
	_, fb := e.doJSON("GET", "/api/flats?keyword=flat1", nil)
	flatList := dataOf(t, fb)["list"].([]interface{})
	if len(flatList) != 1 {
		t.Fatalf("应恰好 1 条 flat1 站点源: %v", fb)
	}
	flatID := flatList[0].(map[string]interface{})["id"].(string)

	// 父源禁用 → 关联站点源同步失效，订阅不含
	e.doJSON("POST", "/api/sources/"+idA+"/toggle", nil)
	if subContains("flat1.example") {
		t.Fatal("父源禁用后订阅不应含 flat1")
	}
	// 父源恢复 → 自动恢复（不覆盖单独状态）
	e.doJSON("POST", "/api/sources/"+idA+"/toggle", nil)
	if !subContains("flat1.example") {
		t.Fatal("父源恢复后订阅应重新含 flat1")
	}

	// 单独禁用站点源 → 订阅不含；父源切换不影响该单独状态
	e.doJSON("POST", "/api/flats/"+flatID+"/toggle", nil)
	if subContains("flat1.example") {
		t.Fatal("单独禁用后订阅不应含 flat1")
	}
	e.doJSON("POST", "/api/sources/"+idA+"/toggle", nil)
	if subContains("flat1.example") {
		t.Fatal("父源禁用后不应含 flat1")
	}
	e.doJSON("POST", "/api/sources/"+idA+"/toggle", nil)
	if subContains("flat1.example") {
		t.Fatal("父源恢复后，单独禁用的 flat1 仍不应输出")
	}
	// 恢复单独启用
	e.doJSON("POST", "/api/flats/"+flatID+"/toggle", nil)
	if !subContains("flat1.example") {
		t.Fatal("重新启用后订阅应含 flat1")
	}

	// 父源删除：flat1 被单仓C 共享 → 保留但父引用只剩单仓C；flat2 无父 → 删除
	code, body = e.doJSON("POST", "/api/sources", map[string]interface{}{
		"name": "单仓C", "url": "http://flat1.example/a.json", "autoCheck": false,
	})
	if code != 200 {
		t.Fatal("添加共享子仓的单仓失败")
	}
	e.markType(dataOf(t, body)["id"].(string), model.TypeSingle)
	e.doJSON("DELETE", "/api/sources/"+idA, nil)
	_, fb = e.doJSON("GET", "/api/flats?keyword=flat1", nil)
	flatList = dataOf(t, fb)["list"].([]interface{})
	if len(flatList) != 1 {
		t.Fatalf("flat1 还有单仓C 父源，应保留: %v", fb)
	}
	parents := flatList[0].(map[string]interface{})["parentNames"].([]interface{})
	if len(parents) != 1 || parents[0].(string) != "单仓C" {
		t.Fatalf("flat1 父引用应只剩单仓C: %v", parents)
	}
	_, fb = e.doJSON("GET", "/api/flats?keyword=flat2", nil)
	flatList = dataOf(t, fb)["list"].([]interface{})
	if len(flatList) != 0 {
		t.Fatalf("flat2 无父引用应已删除: %v", fb)
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

	// 令牌修改接口已移除：只能通过配置文件改令牌
	if code, _ := e.doJSON("POST", "/api/config/token", map[string]string{"oldToken": e.token, "newToken": "new-token-9"}); code != 404 {
		t.Fatal("令牌修改接口应已移除（404）")
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

// 令牌可通过界面（PUT /api/config 带 token 字段）修改且立即生效：
// 鉴权中间件每次请求都读最新配置，改完旧令牌立刻失效，无需重启。
func TestConfigTokenChange(t *testing.T) {
	e := newEnv(t)
	old := e.token

	// 修改令牌
	_, body := e.doJSON("GET", "/api/config", nil)
	cfg := dataOf(t, body)["config"].(map[string]interface{})
	cfg["token"] = "brand-new-token"
	code, body := e.doJSON("PUT", "/api/config", cfg)
	if code != 200 {
		t.Fatalf("修改令牌失败: %d %v", code, body)
	}
	if e.st.Config().Token != "brand-new-token" {
		t.Fatalf("令牌未生效: %q", e.st.Config().Token)
	}

	// 旧令牌立刻 401，新令牌可用
	if code, _ := e.do("GET", "/api/stats", old, "", ""); code != 401 {
		t.Fatalf("旧令牌应立即失效，实际 %d", code)
	}
	if code, _ := e.do("GET", "/api/stats", "brand-new-token", "", ""); code != 200 {
		t.Fatalf("新令牌应可用，实际 %d", code)
	}

	// 脱敏值 "**" 与空串都表示保持不变
	e.token = "brand-new-token"
	_, body = e.doJSON("GET", "/api/config", nil)
	cfg = dataOf(t, body)["config"].(map[string]interface{})
	cfg["token"] = "**"
	if code, body := e.doJSON("PUT", "/api/config", cfg); code != 200 {
		t.Fatalf("脱敏值应放行: %d %v", code, body)
	}
	if e.st.Config().Token != "brand-new-token" {
		t.Fatalf("** 不应改动令牌: %q", e.st.Config().Token)
	}
	cfg["token"] = ""
	if code, body := e.doJSON("PUT", "/api/config", cfg); code != 200 {
		t.Fatalf("空令牌应放行: %d %v", code, body)
	}
	if e.st.Config().Token != "brand-new-token" {
		t.Fatalf("空值不应改动令牌: %q", e.st.Config().Token)
	}

	// 长度校验：6-64
	cfg["token"] = "abc"
	if code, _ := e.doJSON("PUT", "/api/config", cfg); code != 400 {
		t.Fatalf("过短令牌应 400，实际 %d", code)
	}
	cfg["token"] = strings.Repeat("x", 65)
	if code, _ := e.doJSON("PUT", "/api/config", cfg); code != 400 {
		t.Fatalf("过长令牌应 400，实际 %d", code)
	}
}

// 未设置令牌 = 无鉴权模式：/api/* 全放行，healthz 报告 tokenRequired=false；
// 通过设置接口配置令牌后，下一个请求立即恢复校验。
func TestNoAuthWhenTokenEmpty(t *testing.T) {
	dir := t.TempDir()
	st, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if st.Config().Token != "" {
		t.Fatalf("未配置时不应有令牌，实际 %q", st.Config().Token)
	}
	cfg := st.Config()
	cfg.StartupCheck = false
	sch := scheduler.Start(st, &cfg)
	defer sch.Shutdown()
	webFS := fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("<html>ok</html>")}}
	api := New(st, sch, "test", webFS)
	api.PastedDir = t.TempDir() // 粘贴单仓托管目录
	handler := api.Router()

	do := func(method, path, token string) int {
		req := httptest.NewRequest(method, path, nil)
		if token != "" {
			req.Header.Set("X-Token", token)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec.Code
	}

	// 无鉴权模式：不带令牌直接放行
	if code := do("GET", "/api/stats", ""); code != 200 {
		t.Fatalf("无鉴权模式应放行 /api/stats，实际 %d", code)
	}

	// healthz 报告 tokenRequired=false
	req := httptest.NewRequest("GET", "/healthz", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	var h map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &h)
	if h["tokenRequired"] != false {
		t.Fatalf("tokenRequired 应为 false: %v", h)
	}

	// 无鉴权模式下通过设置接口配置令牌（空 / "**" 保持不变，带新值即设置）
	body := `{"port":8080,"subPath":"/sub","checkIntervalMinutes":360,"checkTimeoutSeconds":15,"checkConcurrency":10,"token":"brand-new-token"}`
	req = httptest.NewRequest("PUT", "/api/config", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("设置令牌失败: %d %s", rec.Code, rec.Body.String())
	}

	// 设置后立即恢复校验：无 token 401，新 token 200
	if code := do("GET", "/api/stats", ""); code != 401 {
		t.Fatalf("设置令牌后无 token 应 401，实际 %d", code)
	}
	if code := do("GET", "/api/stats", "brand-new-token"); code != 200 {
		t.Fatalf("新令牌应 200，实际 %d", code)
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

// 改父源地址后必须清掉旧的解析结果：否则 Type 还是 multi、Detail 还是老仓库的子仓，
// 站点源会继续按旧子仓地址生成，新地址反而进不了订阅。
// 改地址时要和新增一样查重：两条源同 URL 会被合成一条站点源，
// 之后删掉其中任意一条都会把共享的站点源一起摘掉。
func TestUpdateSourceDuplicateURL(t *testing.T) {
	e := newEnv(t)
	_, b1 := e.doJSON("POST", "/api/sources", map[string]interface{}{
		"name": "甲", "url": "http://dup.example/a.json", "autoCheck": false,
	})
	idA := dataOf(t, b1)["id"].(string)
	_, b2 := e.doJSON("POST", "/api/sources", map[string]interface{}{
		"name": "乙", "url": "http://dup.example/b.json", "autoCheck": false,
	})
	idB := dataOf(t, b2)["id"].(string)

	// 乙改成甲已占用的地址 → 409
	code, body := e.doJSON("PUT", "/api/sources/"+idB, map[string]interface{}{"url": "http://dup.example/a.json"})
	if code != 409 {
		t.Fatalf("改成已有地址应 409，实际 %d %v", code, body)
	}
	// 原地址未被改动
	if s, _ := e.st.Get(idB); s.URL != "http://dup.example/b.json" {
		t.Errorf("冲突时不应改动原地址，实际 %s", s.URL)
	}

	// 归一化后等价也算冲突（大小写 host / 尾斜杠）
	code, _ = e.doJSON("PUT", "/api/sources/"+idB, map[string]interface{}{"url": "HTTP://DUP.EXAMPLE/a.json"})
	if code != 409 {
		t.Errorf("归一化后相同也应 409，实际 %d", code)
	}

	// 自己改自己（地址没变）应放行
	code, body = e.doJSON("PUT", "/api/sources/"+idB, map[string]interface{}{
		"url": "http://dup.example/b.json", "name": "乙改",
	})
	if code != 200 {
		t.Fatalf("地址未变应放行，实际 %d %v", code, body)
	}

	// 改成没人用的地址应放行
	code, _ = e.doJSON("PUT", "/api/sources/"+idA, map[string]interface{}{"url": "http://dup.example/c.json"})
	if code != 200 {
		t.Errorf("改成空闲地址应 200，实际 %d", code)
	}
}

func TestUpdateSourceURLDropsStaleDetail(t *testing.T) {
	e := newEnv(t)
	_, body := e.doJSON("POST", "/api/sources", map[string]interface{}{
		"name": "老多仓", "url": "http://old.example/all.json", "autoCheck": false,
	})
	id := dataOf(t, body)["id"].(string)

	// 构造「已探活成功、类型 multi」的状态（相当于这个源刚被检测过）
	if err := e.st.Update(id, func(sp *model.Source) {
		sp.Type = model.TypeMulti
		sp.Detail = &model.Detail{SubURLs: []model.SubURL{
			{Name: "旧子仓", URL: "http://old.example/1.json"},
		}}
	}); err != nil {
		t.Fatal(err)
	}

	// 换成完全不同的单仓地址
	code, body := e.doJSON("PUT", "/api/sources/"+id, map[string]interface{}{"url": "http://new.example/9.json"})
	if code != 200 {
		t.Fatalf("改地址失败: %d %v", code, body)
	}
	got := dataOf(t, body)
	if got["type"] != model.TypeUnknown {
		t.Errorf("改地址后类型应重置为 unknown，实际 %v", got["type"])
	}
	if got["detail"] != nil {
		t.Errorf("改地址后旧解析结果应清空，实际 %v", got["detail"])
	}

	_, body = e.doJSON("GET", "/api/flats?size=50", nil)
	list, _ := dataOf(t, body)["list"].([]interface{})
	if len(list) != 0 {
		t.Fatalf("改地址后未探活前不应有站点源，实际 %d: %v", len(list), list)
	}

	// 探活确认新地址是单仓 → 站点源跟着换成新地址（旧子仓不会被带过来）
	e.markType(id, model.TypeSingle)
	_, body = e.doJSON("GET", "/api/flats?size=50", nil)
	list, _ = dataOf(t, body)["list"].([]interface{})
	if len(list) != 1 {
		t.Fatalf("确认后应只有 1 条站点源，实际 %d: %v", len(list), list)
	}
	if url := list[0].(map[string]interface{})["url"]; url != "http://new.example/9.json" {
		t.Errorf("站点源没跟着换地址，仍是 %v", url)
	}
}

// 订阅接口必须去重：多仓子仓里同一个地址写了两种写法（只差尾斜杠），
// 旧版本会生成两条站点源并在订阅里下发两次，现在只应出现一条。
func TestSubDedupURLVariants(t *testing.T) {
	e := newEnv(t)
	code, body := e.doJSON("POST", "/api/sources", map[string]interface{}{
		"name": "聚合", "url": "http://multi.example/d.json", "autoCheck": false,
	})
	if code != 200 {
		t.Fatalf("添加多仓源失败: %v", body)
	}
	id := dataOf(t, body)["id"].(string)
	e.st.Update(id, func(sp *model.Source) {
		sp.Type = model.TypeMulti
		sp.Detail = &model.Detail{SubURLs: []model.SubURL{
			{Name: "毒盒", URL: "https://tv.example/tv"},
			{Name: "毒盒线路", URL: "https://tv.example/tv/"},
		}}
	})

	code, bodyStr := e.do("GET", "/sub", "", "", "")
	if code != 200 {
		t.Fatal("/sub 失败")
	}
	var sub map[string]interface{}
	if err := json.Unmarshal([]byte(bodyStr), &sub); err != nil {
		t.Fatalf("解析 /sub 失败: %v", err)
	}
	urls := sub["urls"].([]interface{})
	if len(urls) != 1 {
		t.Fatalf("同一地址的两种写法应只下发 1 条，实际 %d 条: %s", len(urls), bodyStr)
	}

	// 纯文本口径同样去重
	_, txt := e.do("GET", "/sub/txt", "", "", "")
	if n := len(strings.Split(strings.TrimSpace(txt), "\n")); n != 1 {
		t.Errorf("/sub/txt 应只有 1 行，实际 %d 行: %q", n, txt)
	}
}

// 出口兜底：即使数据层混进了同一地址的不同写法，也只下发第一条（保持顺序）。
func TestDedupFlats(t *testing.T) {
	in := []model.FlatSource{
		{ID: "1", URL: "https://d.example/tv"},
		{ID: "2", URL: "https://d.example/tv/"},  // 尾斜杠
		{ID: "3", URL: "https://d.example/tv2"},  // 真不同
		{ID: "4", URL: "https://d.example/tv#x"}, // fragment
	}
	out := dedupFlats(in)
	if len(out) != 2 {
		t.Fatalf("应去重为 2 条，实际 %d 条", len(out))
	}
	if out[0].ID != "1" || out[1].ID != "3" {
		t.Errorf("应保留首次出现的条目并保持顺序，实际 %s,%s", out[0].ID, out[1].ID)
	}
}

// 端到端复现用户操作路径：设置页保存「连续失败自动禁用」→ 添加源 →
// 手动连续探活 → 达到阈值后源应自动禁用。
func TestAutoDisableEndToEnd(t *testing.T) {
	e := newEnv(t)
	fail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	}))
	defer fail.Close()

	// 1. 设置页保存配置：连续失败 2 次自动禁用
	code, body := e.doJSON("PUT", "/api/config", map[string]interface{}{
		"port": 8080, "subPath": "/sub",
		"checkIntervalMinutes": 360, "checkTimeoutSeconds": 5, "checkConcurrency": 5,
		"startupCheck": false, "autoDisableAfterFails": 2,
		"insecureSkipVerify": true, "checkSubUrls": false,
	})
	if code != 200 {
		t.Fatalf("保存配置失败: %v", body)
	}
	if got := e.st.Config().AutoDisableAfterFails; got != 2 {
		t.Fatalf("配置应保存为 2，实际 %d", got)
	}

	// 2. 添加一个永远 500 的源
	code, b1 := e.doJSON("POST", "/api/sources", map[string]interface{}{
		"name": "坏源", "url": fail.URL + "/1.json", "autoCheck": false,
	})
	if code != 200 {
		t.Fatalf("添加源失败: %v", b1)
	}
	id := dataOf(t, b1)["id"].(string)

	// 3. 连续手动探活 3 轮（阈值 2，第 2 轮就应禁用；多跑 1 轮确认不恢复）
	for round := 1; round <= 3; round++ {
		e.sch.RunAll("test") // 同步跑完一轮
	}

	// 4. 断言：源已被自动禁用，且状态为 fail
	s, _ := e.st.Get(id)
	if s.Enabled {
		t.Errorf("连续失败 %d 次应自动禁用，FailStreak=%d, msg=%q", 3, s.FailStreak, s.CheckMsg)
	}
	if s.Status != model.StatusFail {
		t.Errorf("状态应为 fail，实际 %q", s.Status)
	}
	if s.FailStreak < 2 {
		t.Errorf("FailStreak 应 >= 2，实际 %d", s.FailStreak)
	}
}

// 每条源单独的检测超时：编辑接口要能保存并回读（0 = 用全局）。
func TestSourceTimeoutSeconds(t *testing.T) {
	e := newEnv(t)
	code, b1 := e.doJSON("POST", "/api/sources", map[string]interface{}{
		"name": "大仓库", "url": e.mock.URL + "/big.json", "autoCheck": false, "timeoutSeconds": 45,
	})
	if code != 200 {
		t.Fatalf("添加失败: %v", b1)
	}
	id := dataOf(t, b1)["id"].(string)
	if s, _ := e.st.Get(id); s.TimeoutSeconds != 45 {
		t.Fatalf("创建时应保存 timeoutSeconds=45，实际 %d", s.TimeoutSeconds)
	}
	// 编辑改成 0（回到全局）
	if code, _ := e.doJSON("PUT", "/api/sources/"+id, map[string]interface{}{"timeoutSeconds": 0}); code != 200 {
		t.Fatal("编辑失败")
	}
	if s, _ := e.st.Get(id); s.TimeoutSeconds != 0 {
		t.Fatalf("编辑后应为 0（用全局），实际 %d", s.TimeoutSeconds)
	}

	// 站点源：更新接口同样支持
	e.markType(id, model.TypeSingle) // 单仓 → 生成站点源
	flats := e.st.ListFlats(store.FlatFilter{})
	if len(flats) == 0 {
		t.Fatal("应生成站点源")
	}
	fid := flats[0].ID
	if flats[0].TimeoutSeconds != 0 {
		t.Fatalf("站点源创建时应继承父源的 0，实际 %d", flats[0].TimeoutSeconds)
	}
	// 父源改为 30 后，新同步不应覆盖已存在条目的单独设置
	if code, _ := e.doJSON("PUT", "/api/sources/"+id, map[string]interface{}{"timeoutSeconds": 30}); code != 200 {
		t.Fatal("编辑失败")
	}
	if code, _ := e.doJSON("PUT", "/api/flats/"+fid, map[string]interface{}{"timeoutSeconds": 60}); code != 200 {
		t.Fatal("站点源编辑失败")
	}
	if f, _ := e.st.GetFlat(fid); f.TimeoutSeconds != 60 {
		t.Fatalf("站点源应保存 60，实际 %d", f.TimeoutSeconds)
	}
}

// 用户报障场景：点单条「检测」，连续失败达到阈值也必须自动禁用——
// 此前禁用判断只挂在全量探活里，手动检测路径只累计次数从不禁用。
func TestAutoDisableViaManualCheck(t *testing.T) {
	e := newEnv(t)
	fail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer fail.Close()

	if code, b := e.doJSON("PUT", "/api/config", map[string]interface{}{
		"port": 8080, "subPath": "/sub",
		"checkIntervalMinutes": 360, "checkTimeoutSeconds": 5, "checkConcurrency": 5,
		"startupCheck": false, "autoDisableAfterFails": 2,
		"insecureSkipVerify": true, "checkSubUrls": false,
	}); code != 200 {
		t.Fatalf("保存配置失败: %v", b)
	}

	code, b1 := e.doJSON("POST", "/api/sources", map[string]interface{}{
		"name": "坏源", "url": fail.URL + "/1.json", "autoCheck": false,
	})
	if code != 200 {
		t.Fatalf("添加失败: %v", b1)
	}
	id := dataOf(t, b1)["id"].(string)

	// 手动点两次「检测」（阈值 2），每次等异步探活落定
	for i := 0; i < 2; i++ {
		if code, b := e.do("POST", "/api/sources/"+id+"/check", e.token, "", ""); code != 200 {
			t.Fatalf("第 %d 次检测触发失败: %v", i+1, b)
		}
		for j := 0; j < 300; j++ {
			if s, _ := e.st.Get(id); s.Status != model.StatusChecking {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	s, _ := e.st.Get(id)
	if s.Enabled {
		t.Errorf("手动检测连续失败 2 次应自动禁用: streak=%d, msg=%q", s.FailStreak, s.CheckMsg)
	}
	if !strings.Contains(s.CheckMsg, "已自动禁用") {
		t.Errorf("检测信息应注明自动禁用: %q", s.CheckMsg)
	}
}
