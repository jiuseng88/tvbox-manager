package parser

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"tvbox-manager/internal/model"
)

// CleanJSON：整体非 JSON、但里面嵌着正常 JSON 段（前后有杂文字）时要能提取出来
func TestCleanJSONEmbedded(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string // 提取并清洗后的 JSON（判等用字符串）
	}{
		{
			name: "前后杂文包住对象",
			in:   "更新时间 2026-10-28\n接口如下：\n{\"urls\":[{\"url\":\"http://a/1.json\"}]}\n以上内容转载自网络",
			want: `{"urls":[{"url":"http://a/1.json"}]}`,
		},
		{
			name: "前后杂文包住数组",
			in:   "公告：\n[\"http://a/1.json\",\"http://b/2.json\"]\n（完）",
			want: `["http://a/1.json","http://b/2.json"]`,
		},
		{
			name: "杂文里的干扰括号不干扰",
			in:   "【注意】{转发请注明}来源如下：\n{\"sites\":[{\"name\":\"A\"}]}\n完",
			want: `{"sites":[{"name":"A"}]}`,
		},
		{
			name: "JSON 字符串内的方括号不破坏配平",
			in:   "xxx\n{\"sites\":[{\"name\":\"A[1]组\",\"api\":\"http://x?a=[1]\"}]}\nyyy",
			want: `{"sites":[{"name":"A[1]组","api":"http://x?a=[1]"}]}`,
		},
		{
			name: "嵌入 JSONC（注释+尾逗号）",
			in:   "// 说明\n前缀文字\n{\n// 注释\n\"sites\":[{\"name\":\"A\"},],\n}\n后缀",
			want: `{"sites":[{"name":"A"}]}`,
		},
		{
			name: "标准 JSON 原样保留",
			in:   `{"a":1}`,
			want: `{"a":1}`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := CleanJSON([]byte(c.in))
			if !json.Valid(got) {
				t.Fatalf("结果不是合法 JSON: %s", got)
			}
			// 规范化后比较（行注释会保留换行，空白差异不参与判等）
			var cb, wb bytes.Buffer
			if err := json.Compact(&cb, got); err != nil {
				t.Fatalf("清洗结果不合法: %v (%s)", err, got)
			}
			if err := json.Compact(&wb, []byte(c.want)); err != nil {
				t.Fatal(err)
			}
			if cb.String() != wb.String() {
				t.Fatalf("\n got  %s\n want %s", cb.String(), wb.String())
			}
		})
	}
}

// 前导杂文里出现成对花括号（如 {广告}）时，首个起点配平成功但不合法，
// 要能继续尝试直到找到真正合法的那段
func TestCleanJSONEmbeddedMultipleCandidates(t *testing.T) {
	in := "前缀 {这} 是杂文 {以及这个}，真正的配置：\n{\"urls\":[{\"url\":\"http://a/1.json\",\"name\":\"甲\"}]}\n后缀 {完}"
	got := string(CleanJSON([]byte(in)))
	if !strings.Contains(got, "urls") {
		t.Fatalf("未提取到真正的 JSON 段: %s", got)
	}
	if !json.Valid([]byte(got)) {
		t.Fatalf("结果不是合法 JSON: %s", got)
	}
}

// CleanJSON：字符串内夹带裸控制字符（社区源人肉编辑的常见事故，
// 如 fxz.json 的 "./lib/PG.txt;md5; c84080…\n"、4k.json 的 "优\n酷"）
func TestCleanJSONRawControlInString(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "值尾部的裸换行",
			in:   "{\"api\": \"./lib/PG.txt;md5; c840802d" + "\n" + "\",\"type\":1}",
			want: `{"api":"./lib/PG.txt;md5; c840802d","type":1}`,
		},
		{
			name: "名字中间的裸换行",
			in:   "{\"sites\":[{\"name\":\"优\n酷\",\"type\":1}],\"lives\":[{\"name\":\"芒\n果\"}]}",
			want: `{"sites":[{"name":"优酷","type":1}],"lives":[{"name":"芒果"}]}`,
		},
		{
			name: "字符串内的制表符转空格",
			in:   "{\"name\":\"A\tB\"}",
			want: `{"name":"A B"}`,
		},
		{
			name: "已转义的换行不受影响",
			in:   "{\"ext\":\"{\\n  \\\"免嗅\\\": \\\"1\\\"\\n}\"}",
			want: `{"ext":"{\n  \"免嗅\": \"1\"\n}"}`,
		},
		{
			name: "URL 里的双斜杠不受影响",
			in:   "{\"api\":\"https://jihulab.com/a/b.js\"}",
			want: `{"api":"https://jihulab.com/a/b.js"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := CleanJSON([]byte(tc.in))
			var m map[string]interface{}
			if err := json.Unmarshal(got, &m); err != nil {
				t.Fatalf("清洗后仍不合法: %v\n输入: %q\n输出: %q", err, tc.in, got)
			}
			if norm := compact(t, got); norm != tc.want {
				t.Fatalf("结果不符\n got: %s\nwant: %s", norm, tc.want)
			}
		})
	}
}

// CleanJSON：数组元素缺开引号（fxz.json 实测连片出现：
// "其他剧",日韩剧",其他片", —— 一遍式修复会顾此失彼，必须迭代）
func TestCleanJSONMissingOpenQuote(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "单个缺开引号",
			in:   `{"categories":["动作片",日韩剧","喜剧片"]}`,
			want: `{"categories":["动作片","日韩剧","喜剧片"]}`,
		},
		{
			name: "连片缺开引号",
			in:   `{"categories":["动作片",日韩剧",其他片",歌舞片"]}`,
			want: `{"categories":["动作片","日韩剧","其他片","歌舞片"]}`,
		},
		{
			name: "裸词直接到收尾",
			in:   `{"categories":["动作片",日韩剧]}`,
			want: `{"categories":["动作片","日韩剧"]}`,
		},
		{
			name: "缩进换行夹杂",
			in:   "{\n\"categories\": [\n\"动作片\",\n日韩剧\",\n\"喜剧片\"\n]\n}",
			want: `{"categories":["动作片","日韩剧","喜剧片"]}`,
		},
		{
			name: "合法字面量不误伤",
			in:   `{"a":[true,false,null,1,-2]}`,
			want: `{"a":[true,false,null,1,-2]}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := CleanJSON([]byte(tc.in))
			var m map[string]interface{}
			if err := json.Unmarshal(got, &m); err != nil {
				t.Fatalf("清洗后仍不合法: %v\n输出: %q", err, got)
			}
			if norm := compact(t, got); norm != tc.want {
				t.Fatalf("结果不符\n got: %s\nwant: %s", norm, tc.want)
			}
		})
	}
}

// 组合伤：注释 + 裸换行 + 缺引号同时存在（对应 fxz.json 的真实形态）
func TestCleanJSONCombinedDamage(t *testing.T) {
	in := "{\n// 每日一言\n\"sites\": [{\"key\":\"a\",\"name\":\"优\n酷\",\"type\":3}],\n" +
		"\"parses\": [],\n\"categories\": [\"电影\",日韩剧\",\"综艺\"],\n\"lives\": []\n}"
	got := CleanJSON([]byte(in))
	typ, detail, err := Parse(got)
	if err != nil {
		t.Fatalf("组合伤应能解析: %v\n输出: %q", err, got)
	}
	if typ != model.TypeSingle || detail.Sites != 1 {
		t.Fatalf("应识别为单仓 1 站点，得到 %s %+v", typ, detail)
	}
	if detail.SiteNames[0] != "优酷" {
		t.Fatalf("名字里的裸换行应被清掉，得到 %q", detail.SiteNames[0])
	}
}

// compact 规范化 JSON（键序不变，仅压空白），便于判等
func compact(t *testing.T, b []byte) string {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Compact(&buf, b); err != nil {
		t.Fatalf("compact 失败: %v (输入 %q)", err, b)
	}
	return buf.String()
}
