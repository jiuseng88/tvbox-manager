package live

import (
	"net/url"
	"testing"

	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/transform"
)

func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", s, err)
	}
	return u
}

func TestParseM3U(t *testing.T) {
	body := `#EXTM3U
#EXTINF:-1 tvg-id="cctv1" tvg-name="CCTV1" tvg-logo="http://logo/1.png" group-title="央视",CCTV-1 综合
http://a.example/cctv1.m3u8
#EXTINF:-1 group-title="卫视",湖南卫视
./hunan/index.m3u8
#EXTGRP:地方
#EXTINF:-1,北京卫视
http://b.example/bj.m3u8
`
	got := Parse([]byte(body), mustURL(t, "http://a.example/live/list.m3u"), "srcA")
	if len(got) != 3 {
		t.Fatalf("频道数应为 3，实际 %d: %+v", len(got), got)
	}
	if got[0].Group != "央视" || got[0].Name != "CCTV-1 综合" || got[0].Logo != "http://logo/1.png" {
		t.Errorf("首条解析错误: %+v", got[0])
	}
	// 相对地址要按清单地址补全
	if got[1].URLs[0] != "http://a.example/live/hunan/index.m3u8" {
		t.Errorf("相对地址未补全: %s", got[1].URLs[0])
	}
	if got[1].Group != "卫视" {
		t.Errorf("第二个分组错误: %s", got[1].Group)
	}
	// #EXTGRP 之后应沿用新分组
	if got[2].Group != "地方" {
		t.Errorf("EXTGRP 分组未生效: %s", got[2].Group)
	}
}

func TestParseTXT(t *testing.T) {
	body := `央视频道,#genre#
CCTV-1,http://a.example/cctv1.m3u8
CCTV-2,http://a.example/cctv2.m3u8#http://b.example/cctv2.m3u8
卫视频道,#genre#
湖南卫视,http://a.example/hunan.m3u8?token=1,2
`
	got := Parse([]byte(body), nil, "srcB")
	if len(got) != 3 {
		t.Fatalf("频道数应为 3，实际 %d: %+v", len(got), got)
	}
	if got[0].Group != "央视频道" || got[0].Name != "CCTV-1" {
		t.Errorf("分组/名称错误: %+v", got[0])
	}
	// 多线路合并成 URLs 两条
	if len(got[1].URLs) != 2 {
		t.Errorf("多线路应合并为 2 条，实际 %v", got[1].URLs)
	}
	// 名称后面带逗号的地址不能被撕碎
	if got[2].URLs[0] != "http://a.example/hunan.m3u8?token=1,2" {
		t.Errorf("地址被逗号撕碎: %s", got[2].URLs[0])
	}
	if got[2].Group != "卫视频道" {
		t.Errorf("第二个分组错误: %s", got[2].Group)
	}
}

func TestParseGBK(t *testing.T) {
	raw := "央视,#genre#\nCCTV-1,http://a.example/1.m3u8\n"
	gbk, _, err := transform.Bytes(simplifiedchinese.GB18030.NewEncoder(), []byte(raw))
	if err != nil {
		t.Fatalf("构造 GBK 样本失败: %v", err)
	}
	got := Parse(gbk, nil, "gbk")
	if len(got) != 1 || got[0].Group != "央视" || got[0].Name != "CCTV-1" {
		t.Fatalf("GBK 解码失败: %+v", got)
	}
}

func TestSplitHeader(t *testing.T) {
	stem, hdr := SplitHeader("http://a.example/1.m3u8|User-Agent=Mozilla&Referer=http://a.example/")
	if stem != "http://a.example/1.m3u8" {
		t.Errorf("stem 错误: %s", stem)
	}
	if hdr != "User-Agent=Mozilla&Referer=http://a.example/" {
		t.Errorf("header 错误: %s", hdr)
	}
}

func TestResolveKeepsHeaderParam(t *testing.T) {
	got := Parse([]byte("#EXTM3U\n#EXTINF:-1,A\nhttp://a.example/1.m3u8|Referer=http://a.example/\n"), nil, "s")
	if len(got) != 1 || got[0].URLs[0] != "http://a.example/1.m3u8|Referer=http://a.example/" {
		t.Fatalf("请求头参数被丢弃: %+v", got)
	}
}

func TestMergeSameChannel(t *testing.T) {
	in := []Channel{
		{Group: "央视", Name: "CCTV-1", URLs: []string{"http://a/1.m3u8"}, Source: "s1"},
		{Group: "央视", Name: "CCTV-1", URLs: []string{"http://b/1.m3u8"}, Source: "s2"},
		{Group: "央视", Name: "CCTV-1", URLs: []string{"http://a/1.m3u8"}, Source: "s3"},
	}
	out := Merge(in)
	if len(out) != 1 {
		t.Fatalf("应合并为 1 条，实际 %d", len(out))
	}
	if len(out[0].URLs) != 2 {
		t.Fatalf("线路应去重为 2 条，实际 %v", out[0].URLs)
	}
	// 来源名要累积去重（s1/s2/s3），否则前端看不出频道来自哪些直播源
	if len(out[0].Sources) != 3 {
		t.Fatalf("来源应累积为 3 个，实际 %v", out[0].Sources)
	}
	if out[0].Source != "s1" {
		t.Fatalf("Source 应保留首个来源 s1，实际 %q", out[0].Source)
	}
}

func TestMergeSourceDedup(t *testing.T) {
	in := []Channel{
		{Group: "央视", Name: "CCTV-1", URLs: []string{"http://a/1.m3u8"}, Source: "s1"},
		{Group: "央视", Name: "CCTV-1", URLs: []string{"http://c/1.m3u8"}, Source: "s1"},
		{Group: "央视", Name: "CCTV-1", URLs: []string{"http://d/1.m3u8"}, Source: ""},
	}
	out := Merge(in)
	if len(out) != 1 {
		t.Fatalf("应合并为 1 条，实际 %d", len(out))
	}
	if len(out[0].Sources) != 1 || out[0].Sources[0] != "s1" {
		t.Fatalf("重复来源应去重、空来源应忽略，实际 %v", out[0].Sources)
	}
}

func TestParseJSONChannels(t *testing.T) {
	body := `{"channels":[{"group":"央视","name":"CCTV-1","url":"http://a/1.m3u8"},{"name":"CCTV-2","urls":["http://a/2.m3u8"]}]}`
	got := Parse([]byte(body), nil, "j")
	if len(got) != 2 {
		t.Fatalf("JSON 解析失败: %+v", got)
	}
	if got[1].Group != DefaultGroup {
		t.Errorf("缺省分组应为 %s，实际 %s", DefaultGroup, got[1].Group)
	}
}

func TestDropUnplayable(t *testing.T) {
	body := `#EXTM3U
#EXTINF:-1,虎牙
proxy://do=live&type=txt
#EXTINF:-1,rtsp台
rtsp://a.example/1
#EXTINF:-1,正常
http://a.example/ok.m3u8
`
	got := Parse([]byte(body), nil, "s")
	if len(got) != 1 || got[0].Name != "正常" {
		t.Fatalf("应只保留 http 频道: %+v", got)
	}
}
