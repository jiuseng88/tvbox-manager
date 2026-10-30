package api

import "testing"

func TestParseLiveLines(t *testing.T) {
	text := "CCTV1,http://a.example/1.m3u8\n" +
		"央视2 http://a.example/2.m3u8\n" +
		"http://a.example/3.m3u8\n" +
		"带头的|http://a.example/4.m3u8|User-Agent=Mozilla\n" +
		"# 注释行\n" +
		"\n" +
		"proxy://do=live\n" +
		"坏行没有地址"
	got := parseLiveLines(text)
	if len(got) != 6 {
		t.Fatalf("应解析出 6 行（含无效），实际 %d: %+v", len(got), got)
	}
	expectOK := []bool{true, true, true, true, false, false}
	for i, want := range expectOK {
		if got[i].OK != want {
			t.Errorf("第 %d 行 ok 应为 %v: %+v", i+1, want, got[i])
		}
	}
	if got[0].Name != "CCTV1" || got[0].URL != "http://a.example/1.m3u8" {
		t.Errorf("逗号格式解析错误: %+v", got[0])
	}
	if got[1].Name != "央视2" || got[1].URL != "http://a.example/2.m3u8" {
		t.Errorf("空格格式解析错误: %+v", got[1])
	}
	if got[2].Name != "" || got[2].URL != "http://a.example/3.m3u8" {
		t.Errorf("纯地址解析错误: %+v", got[2])
	}
	// | 是分隔符的情况：名称「带头的」，地址保留请求头参数
	if got[3].Name != "带头的" || got[3].URL != "http://a.example/4.m3u8|User-Agent=Mozilla" {
		t.Errorf("| 格式解析错误: %+v", got[3])
	}
	if got[4].Reason == "" {
		t.Error("proxy:// 应给出拒绝原因")
	}
}

// URL 里的 | 后面不是 http（即请求头参数）时，不能被当成「名称|地址」分隔符
func TestParseLiveLinesHeaderParam(t *testing.T) {
	got := parseLiveLines("http://a.example/5.m3u8|User-Agent=Mozilla&Referer=http://a.example/")
	if len(got) != 1 || !got[0].OK {
		t.Fatalf("应解析为一条纯地址: %+v", got)
	}
	if got[0].URL != "http://a.example/5.m3u8|User-Agent=Mozilla&Referer=http://a.example/" {
		t.Fatalf("请求头参数被吞掉: %s", got[0].URL)
	}
}

func TestNormalizeLiveURL(t *testing.T) {
	// 合法：保留 | 头参数
	u, err := normalizeLiveURL(" http://a.example/x.m3u8 |User-Agent=x ")
	if err != nil || u != "http://a.example/x.m3u8|User-Agent=x" {
		t.Fatalf("归一化错误: %q %v", u, err)
	}
	// 拒绝 rtsp / 空地址 / 纯垃圾
	if _, err := normalizeLiveURL("rtsp://a.example/1"); err == nil {
		t.Error("rtsp 应被拒绝")
	}
	if _, err := normalizeLiveURL(""); err == nil {
		t.Error("空地址应被拒绝")
	}
	if _, err := normalizeLiveURL("随便写的"); err == nil {
		t.Error("垃圾串应被拒绝")
	}
}
