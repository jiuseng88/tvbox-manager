package parser

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
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
