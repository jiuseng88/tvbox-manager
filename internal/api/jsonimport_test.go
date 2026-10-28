package api

import (
	"strings"
	"testing"

	"tvbox-manager/internal/parser"
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

// 解析失败时接口要 400 且信息可见
func TestImportJSONEndpointBadInput(t *testing.T) {
	e := newEnv(t)
	code, body := e.doJSON("POST", "/api/sources/import/json", map[string]interface{}{
		"text": `{"sites":[{"api":"http://x"}],"lives":[{"name":"直播"}]}`,
	})
	if code != 400 {
		t.Fatalf("code=%d 期望 400，body=%v", code, body)
	}
	if msg, _ := body["msg"].(string); !strings.Contains(msg, "仓库内容") {
		t.Fatalf("提示信息不友好: %v", body["msg"])
	}
}
