package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"tvbox-manager/internal/model"
	"tvbox-manager/internal/parser"
	"tvbox-manager/internal/store"
)

func TestParseSourceJSONShapes(t *testing.T) {
	cases := []struct {
		name string
		in   string
		kind string
		want []string // name|url
	}{
		{
			name: "TVBox 分享格式",
			in:   `{"urls":[{"url":"http://a/1.json","name":"甲"},{"url":"http://a/2.json"}]}`,
			kind: "urls",
			want: []string{"甲|http://a/1.json", "|http://a/2.json"},
		},
		{
			name: "字符串数组",
			in:   `["http://a/1.json","https://b/2.json"]`,
			kind: "array",
			want: []string{"|http://a/1.json", "|https://b/2.json"},
		},
		{
			name: "对象数组",
			in:   `[{"name":"乙","url":"http://b/2.json"}]`,
			kind: "array",
			want: []string{"乙|http://b/2.json"},
		},
		{
			name: "备份格式 sources",
			in:   `{"sources":[{"name":"丙","url":"http://c/3.json","group":"客厅","enabled":false}]}`,
			kind: "sources",
			want: []string{"丙|http://c/3.json"},
		},
		{
			name: "单个对象",
			in:   `{"name":"丁","url":"http://d/4.json"}`,
			kind: "single",
			want: []string{"丁|http://d/4.json"},
		},
		{
			name: "包装层 data.urls",
			in:   `{"code":0,"data":{"urls":[{"url":"http://e/5.json"}]}}`,
			kind: "scan",
			want: []string{"|http://e/5.json"},
		},
		{
			name: "包装层兜底深扫",
			in:   `{"payload":{"children":[{"url":"http://f/6.json","title":"戊"}]}}`,
			kind: "scan",
			want: []string{"戊|http://f/6.json"},
		},
		{
			name: "同址去重",
			in:   `{"urls":[{"url":"http://a/1.json"},{"url":"HTTP://A/1.json"}]}`,
			kind: "urls",
			want: []string{"|http://a/1.json"},
		},
		{
			name: "名称含逗号不被切坏",
			in:   `{"urls":[{"url":"http://a/1.json","name":"甲,乙"}]}`,
			kind: "urls",
			want: []string{"甲,乙|http://a/1.json"},
		},
		{
			name: "URL 内的 // 与 /* 不被当注释",
			in:   `{"urls":[{"url":"http://a//b.json?x=1/*2"},{"url":"http://c/d.json"}]}`,
			kind: "urls",
			want: []string{"|http://a//b.json?x=1/*2", "|http://c/d.json"},
		},
		{
			name: "JSONC 行注释 + 块注释 + 悬挂逗号",
			in: "// TVBox 常用源\n" +
				"/* 说明：仅测试 */\n" +
				"{\n" +
				"  // 分享格式\n" +
				"  \"urls\": [\n" +
				"    {\"url\": \"http://a/1.json\", \"name\": \"甲\"}, // 行尾注释\n" +
				"    {\"url\": \"http://a/2.json\", /* 名称见下 */ \"name\": \"乙\"},\n" +
				"  ],\n" +
				"}",
			kind: "urls",
			want: []string{"甲|http://a/1.json", "乙|http://a/2.json"},
		},
		{
			name: "字符串内的 // 注释写法原样保留",
			in:   `{"urls":[{"url":"http://a/1.json","name":"看这个 // 不是注释"}]}`,
			kind: "urls",
			want: []string{"看这个 // 不是注释|http://a/1.json"},
		},
		{
			name: "转义引号里的 // 保留",
			in:   `{"urls":[{"url":"http://a/1.json","name":"说 \"http://x\" 不是注释"}]}`,
			kind: "urls",
			want: []string{`说 "http://x" 不是注释|http://a/1.json`},
		},
		{
			name: "注释里出现 url 键不算数",
			in:   "{\n// {\"url\": \"http://fake/0.json\"}\n\"urls\":[{\"url\":\"http://a/1.json\"}]\n}",
			kind: "urls",
			want: []string{"|http://a/1.json"},
		},
		{
			name: "忽略仓库里的 sites/lives",
			in:   `{"urls":[{"url":"http://a/1.json"}],"sites":[{"api":"http://x/api.php"}],"lives":[{"urls":["http://live/1.m3u8"]}]}`,
			kind: "urls",
			want: []string{"|http://a/1.json"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseSourceJSON(c.in)
			if err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			if got.Kind != c.kind {
				t.Fatalf("kind=%q 期望 %q", got.Kind, c.kind)
			}
			if len(got.Entries) != len(c.want) {
				t.Fatalf("条目数 %d 期望 %d：%+v", len(got.Entries), len(c.want), got.Entries)
			}
			for i, w := range c.want {
				gotStr := got.Entries[i].Name + "|" + got.Entries[i].URL
				if !strings.EqualFold(gotStr, w) {
					t.Fatalf("第 %d 条 = %q 期望 %q", i, gotStr, w)
				}
			}
		})
	}
}

// 清洗函数本身的边界行为：标准 JSON 必须逐字节透传
func TestSanitizeJSONText(t *testing.T) {
	std := `{"urls":[{"url":"http://a/1.json","name":"甲"}],"note":"含 // 和 /* 都不能动"}`
	if got := sanitizeJSONText(std); got != std {
		t.Fatalf("标准 JSON 被改动：\n got  %q\n want %q", got, std)
	}
	cases := []struct{ name, in, want string }{
		{"行注释 EOF", `{"a":1}// 尾注`, `{"a":1}`},
		{"未闭合块注释", `{"a":1} /* 未闭合`, `{"a":1}  `},
		{"悬挂逗号跨行", "[1,2\n,]\n", "[1,2\n]\n"},
		{"字符串内悬挂逗号形态", `{"a":"x,}"}`, `{"a":"x,}"}`},
		{"转义引号后出串", `{"a":"x\"","b":1,}`, `{"a":"x\"","b":1}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := sanitizeJSONText(c.in); got != c.want {
				t.Fatalf("got %q want %q", got, c.want)
			}
		})
	}
}

func TestParseSourceJSONErrors(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"空内容", "   ", "内容为空"},
		{"JSON 被截断", `{"urls":[{"url":"http://a/1.json"`, "JSON 格式不合法"},
		{"合法 JSON 但没有 url", `{"foo":{"bar":1},"n":2}`, "未识别到任何 url 条目"},
		{"仓库内容", `{"sites":[{"api":"http://x"}],"lives":[{"name":"直播"}]}`, "TVBox 仓库内容"},
		{"没有 url 字段", `{"name":"x","version":"1"}`, "未识别到任何 url 条目"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ParseSourceJSON(c.in)
			if err == nil {
				t.Fatalf("期望报错，实际解析成功")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("错误信息 %q 不含 %q", err.Error(), c.want)
			}
		})
	}
}

// 粘贴 JSON 导入端到端：预览 → 导入 → 再预览（重复）→ 再导入（跳过）
func TestImportJSONEndpoint(t *testing.T) {
	e := newEnv(t)
	url := e.mock.URL + "/tvbox.json"
	text := `{"urls":[{"name":"甲,乙","url":"` + url + `"}]}`

	code, body := e.doJSON("POST", "/api/sources/import/json", map[string]interface{}{
		"text": text, "dryRun": true,
	})
	if code != 200 {
		t.Fatalf("预览失败 code=%d body=%v", code, body)
	}
	d := dataOf(t, body)
	if n := num(d, "total"); n != 1 {
		t.Fatalf("识别条数 %d 期望 1", n)
	}
	if n := num(d, "dup"); n != 0 {
		t.Fatalf("重复条数 %d 期望 0", n)
	}

	code, body = e.doJSON("POST", "/api/sources/import/json", map[string]interface{}{
		"text": text, "group": "粘贴导入",
	})
	if code != 200 {
		t.Fatalf("导入失败 code=%d body=%v", code, body)
	}
	if n := num(dataOf(t, body), "added"); n != 1 {
		t.Fatalf("新增 %d 期望 1", n)
	}

	norm, err := parser.Normalize(url)
	if err != nil {
		t.Fatal(err)
	}
	src, found := e.st.GetByURL(norm)
	if !found {
		t.Fatalf("源未入库: %s", url)
	}
	if src.Name != "甲,乙" {
		t.Fatalf("名称 = %q，期望「甲,乙」（名称里的逗号不应被当成分隔符）", src.Name)
	}
	if src.Group != "粘贴导入" {
		t.Fatalf("分组 = %q，期望「粘贴导入」", src.Group)
	}

	// 再预览：应识别为重复
	code, body = e.doJSON("POST", "/api/sources/import/json", map[string]interface{}{
		"text": text, "dryRun": true,
	})
	if code != 200 {
		t.Fatalf("二次预览失败 code=%d", code)
	}
	if n := num(dataOf(t, body), "dup"); n != 1 {
		t.Fatalf("二次预览重复条数 %d 期望 1", n)
	}

	// 再导入：应全部跳过
	code, body = e.doJSON("POST", "/api/sources/import/json", map[string]interface{}{"text": text})
	if code != 200 {
		t.Fatalf("二次导入失败 code=%d", code)
	}
	d = dataOf(t, body)
	if n := num(d, "added"); n != 0 {
		t.Fatalf("二次导入新增 %d 期望 0", n)
	}
	if sk, _ := d["skipped"].([]interface{}); len(sk) != 1 {
		t.Fatalf("跳过条数 %d 期望 1：%v", len(sk), d["skipped"])
	}
}

// 真正无法解析（找不到任何 JSON 数据）时要明确报错
func TestImportJSONEndpointBadInput(t *testing.T) {
	e := newEnv(t)
	code, body := e.doJSON("POST", "/api/sources/import/json", map[string]interface{}{
		"text": `这段公告里没有任何 JSON 数据，纯文字说明，也没有花括号。`,
	})
	if code != 400 {
		t.Fatalf("code=%d 期望 400，body=%v", code, body)
	}
	if msg, _ := body["msg"].(string); !strings.Contains(msg, "JSON") {
		t.Fatalf("提示信息不友好: %v", body["msg"])
	}
}

// 粘贴单仓配置：预览出统计 → 导入即本服务托管 → 能通过 /configs/ 拉到 → 删源清理文件
func TestImportRepoSinglePaste(t *testing.T) {
	e := newEnv(t)
	text := `更新公告：本站接口如下，请注意保存。` + "\n" +
		`{"spider":"http://x/s.jar","sites":[{"key":"a","name":"A站","type":1,"api":"http://x/api.php"},` +
		`{"key":"b","name":"B站","type":3,"api":"csp_XPath"}],"lives":[{"name":"直播1"}]}` + "\n" +
		`以上内容仅供学习交流。`

	code, body := e.doJSON("POST", "/api/sources/import/json", map[string]interface{}{
		"text": text, "dryRun": true,
	})
	if code != 200 {
		t.Fatalf("预览失败 code=%d body=%v", code, body)
	}
	d := dataOf(t, body)
	if k, _ := d["kind"].(string); k != "repo-single" {
		t.Fatalf("kind=%v 期望 repo-single", d["kind"])
	}
	if s, _ := d["summary"].(string); !strings.Contains(s, "2 站点") || !strings.Contains(s, "1 直播") {
		t.Fatalf("summary=%v", d["summary"])
	}
	// spider 应该被忽略（不在统计/站点名里出现是另一回事，这里校验预览不带地址条目）
	if rows, _ := d["entries"].([]interface{}); len(rows) != 0 {
		t.Fatalf("单仓预览不应有地址条目: %v", d["entries"])
	}

	// 导入 → 生成一条指向 /configs/ 的源
	code, body = e.doJSON("POST", "/api/sources/import/json", map[string]interface{}{
		"text": text, "name": "我的粘贴仓",
	})
	if code != 200 {
		t.Fatalf("导入失败 code=%d body=%v", code, body)
	}
	d = dataOf(t, body)
	selfURL, _ := d["selfHosted"].(string)
	if !strings.Contains(selfURL, "/configs/") || !strings.HasSuffix(selfURL, ".json") {
		t.Fatalf("托管地址异常: %v", selfURL)
	}
	srcID, _ := d["id"].(string)
	src, found := e.st.Get(srcID)
	if !found {
		t.Fatalf("源未入库")
	}
	if src.Name != "我的粘贴仓" || src.URL != selfURL {
		t.Fatalf("源信息不符: %+v", src)
	}
	// 本地源：类型 local、直接可用、无需检测
	if src.Type != model.TypeLocal {
		t.Fatalf("类型 = %q，期望 local", src.Type)
	}
	if src.Status != model.StatusOK {
		t.Fatalf("状态 = %q，期望直接可用", src.Status)
	}

	// 站点源已生成：地址即托管地址、直接可用，并进入订阅输出
	flats := e.st.EnabledFlats()
	var localFlat *model.FlatSource
	for i := range flats {
		if flats[i].URL == selfURL {
			localFlat = &flats[i]
			break
		}
	}
	if localFlat == nil {
		t.Fatalf("本地源站点源未生成")
	}
	if localFlat.Status != model.StatusOK {
		t.Fatalf("站点源状态 = %q，期望直接可用", localFlat.Status)
	}
	code, raw := e.do("GET", "/sub", "", "", "")
	if code != 200 {
		t.Fatalf("订阅拉取失败 code=%d", code)
	}
	if !strings.Contains(raw, selfURL) {
		t.Fatalf("订阅输出应包含托管地址 %s: %s", selfURL, raw)
	}

	// 托管文件能通过无鉴权路由拉到，且内容是清洗后的合法 JSON
	code, raw = e.do("GET", "/configs/"+selfURL[strings.LastIndex(selfURL, "/")+1:], "", "", "")
	if code != 200 {
		t.Fatalf("托管文件拉取失败 code=%d", code)
	}
	var j map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &j); err != nil {
		t.Fatalf("托管内容不是合法 JSON: %v (%s)", err, raw)
	}
	if _, ok := j["sites"]; !ok {
		t.Fatalf("托管内容缺少 sites: %s", raw)
	}

	// 删除源 → 托管文件同步清理
	code, _ = e.do("DELETE", "/api/sources/"+srcID, e.token, "", "")
	if code != 200 {
		t.Fatalf("删除失败 code=%d", code)
	}
	code, _ = e.do("GET", "/configs/"+selfURL[strings.LastIndex(selfURL, "/")+1:], "", "", "")
	if code != 404 {
		t.Fatalf("删除源后托管文件应 404，实际 code=%d", code)
	}
}

// 粘贴多仓配置：只提取子仓地址（名称+URL），spider 等其余内容丢弃
func TestImportRepoMultiPaste(t *testing.T) {
	e := newEnv(t)
	text := `{"spider":"http://x/mix.jar","lives":[{"name":"不该被导入的直播"}],
		"urls":[{"sourceName":"肥猫","sourceUrl":"http://a/1.json"},
		        {"sourceName":"饭太硬","sourceUrl":"http://b/2.json"},
		        {"sourceName":"肥猫重复","sourceUrl":"http://a/1.json"}]}`

	code, body := e.doJSON("POST", "/api/sources/import/json", map[string]interface{}{
		"text": text, "dryRun": true,
	})
	if code != 200 {
		t.Fatalf("预览失败 code=%d body=%v", code, body)
	}
	d := dataOf(t, body)
	if k, _ := d["kind"].(string); k != "repo-multi" {
		t.Fatalf("kind=%v 期望 repo-multi", d["kind"])
	}
	if n := num(d, "total"); n != 2 {
		t.Fatalf("提取条数 %d 期望 2（同址去重）：%v", n, d["entries"])
	}

	code, body = e.doJSON("POST", "/api/sources/import/json", map[string]interface{}{
		"text": text, "group": "多仓粘贴",
	})
	if code != 200 {
		t.Fatalf("导入失败 code=%d body=%v", code, body)
	}
	if n := num(dataOf(t, body), "added"); n != 2 {
		t.Fatalf("新增 %d 期望 2", n)
	}
	src, found := e.st.GetByURL("http://a/1.json")
	if !found {
		t.Fatalf("子仓未入库")
	}
	if src.Name != "肥猫" {
		t.Fatalf("子仓名称 = %q，期望「肥猫」（sourceName 键识别）", src.Name)
	}
	// spider jar 不应被当成源导入
	if _, found := e.st.GetByURL("http://x/mix.jar"); found {
		t.Fatalf("spider 不应被导入")
	}
}

// 前后带杂文、中间嵌着地址列表数组：提取后照常导入
func TestImportJunkWrappedList(t *testing.T) {
	e := newEnv(t)
	text := "更新时间 2026-10-28，接口如下：\n" +
		`["http://a/1.json","http://b/2.json"]` + "\n" +
		"以上内容转载自网络，仅供学习交流。"
	code, body := e.doJSON("POST", "/api/sources/import/json", map[string]interface{}{
		"text": text, "dryRun": true,
	})
	if code != 200 {
		t.Fatalf("预览失败 code=%d body=%v", code, body)
	}
	if n := num(dataOf(t, body), "total"); n != 2 {
		t.Fatalf("识别条数 %d 期望 2", n)
	}
}

// 前后带杂文、中间嵌着单仓配置：同样能识别为 repo-single
func TestImportJunkWrappedSingle(t *testing.T) {
	e := newEnv(t)
	text := "公告：接口更新\n" + `{"sites":[{"key":"a","name":"A站","type":1,"api":"http://x/api.php"}]}` + "\n完"
	code, body := e.doJSON("POST", "/api/sources/import/json", map[string]interface{}{
		"text": text, "dryRun": true,
	})
	if code != 200 {
		t.Fatalf("预览失败 code=%d body=%v", code, body)
	}
	if k, _ := dataOf(t, body)["kind"].(string); k != "repo-single" {
		t.Fatalf("应识别为 repo-single: %v", body)
	}
}

// 探活对非标准 JSON（前后杂文）的兼容：mock 直接返回杂文包住的单仓
func TestProbeToleratesJunkWrappedConfig(t *testing.T) {
	e := newEnv(t)
	// 起真实 HTTP 服务（否则托管地址的 Host 是假的，探活拉不到）
	srv := httptest.NewServer(e.api)
	t.Cleanup(srv.Close)

	post := func(path string, v interface{}) (int, map[string]interface{}) {
		b, _ := json.Marshal(v)
		req, _ := http.NewRequest("POST", srv.URL+path, bytes.NewReader(b))
		req.Header.Set("X-Token", e.token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		rb, _ := io.ReadAll(resp.Body)
		var out map[string]interface{}
		_ = json.Unmarshal(rb, &out)
		return resp.StatusCode, out
	}

	// 托管一份前后带杂文的单仓配置
	text := "公告文字\n" + `{"sites":[{"key":"a","name":"A站","type":1,"api":"` + e.mock.URL + `/api.php"}]}` + "\n尾注"
	code, body := post("/api/sources/import/json", map[string]interface{}{"text": text})
	if code != 200 {
		t.Fatalf("托管失败: %v", body)
	}
	d := dataOf(t, body)
	selfURL, _ := d["selfHosted"].(string)
	srcID, _ := d["id"].(string)

	// 已入库源预览（不做内网拦截）：杂文包着的单仓应被识别，且能看原始返回
	req, _ := http.NewRequest("GET", srv.URL+"/api/sources/"+srcID+"/preview", nil)
	req.Header.Set("X-Token", e.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	var out map[string]interface{}
	_ = json.Unmarshal(rb, &out)
	pd := dataOf(t, out)
	if k, _ := pd["type"].(string); k != "single" {
		t.Fatalf("非标准单仓应被识别为 single: %v", pd)
	}
	rawBody, _ := pd["raw"].(string)
	if !strings.Contains(rawBody, "sites") {
		t.Fatalf("预览应包含原始返回内容: %v", pd)
	}
	_ = selfURL
}

// 订阅接口只看启用开关：检测状态（可用/失效/未知）不影响输出
func TestSubIgnoresStatus(t *testing.T) {
	e := newEnv(t)
	// 直接构造一个已定型的单仓源（绕过异步探活的时序）
	src := model.Source{Name: "甲", URL: e.mock.URL + "/tv.json", Type: model.TypeSingle, Enabled: true}
	if err := e.st.Add(&src); err != nil {
		t.Fatal(err)
	}
	// 模拟探活失败：状态 fail 但仍然启用
	if err := e.st.UpdateCheckResult(src.ID, store.CheckResult{Status: model.StatusFail, Msg: "模拟失效"}, false); err != nil {
		t.Fatal(err)
	}

	code, raw := e.do("GET", "/sub", "", "", "")
	if code != 200 {
		t.Fatalf("订阅拉取失败 code=%d", code)
	}
	if !strings.Contains(raw, e.mock.URL+"/tv.json") {
		t.Fatalf("失效但启用的仓源必须照常下发: %s", raw)
	}

	// 关掉启用开关后才应消失
	if err := e.st.Update(src.ID, func(sp *model.Source) { sp.Enabled = false }); err != nil {
		t.Fatal(err)
	}
	code, raw = e.do("GET", "/sub", "", "", "")
	if code != 200 {
		t.Fatalf("订阅拉取失败 code=%d", code)
	}
	if strings.Contains(raw, e.mock.URL+"/tv.json") {
		t.Fatalf("禁用后不应下发: %s", raw)
	}
}
