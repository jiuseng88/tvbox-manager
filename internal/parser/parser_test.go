package parser

import (
	"strings"
	"testing"
)

// 真实社区源格式样本（字段形态取自真实配置，内容简化）

// realSingle 真实单仓：csp_ 站点 + ext 对象/字符串 + spider 带 ;md5; 后缀 + lives 分组 + parses
const realSingle = `{
  "spider": "https://example.com/spider.jar;md5;e7bb32e4cb99f5e7ad30c5a5f09d7be3",
  "wallpaper": "https://example.com/bg.jpg",
  "sites": [
    {"key": "豆瓣", "name": "豆瓣", "type": 3, "api": "csp_Douban", "searchable": 1},
    {"key": "玩偶", "name": "玩偶哥哥", "type": 3, "api": "csp_XPathMac", "searchable": 1,
     "ext": {"ua": "Mozilla/5.0", "host": "https://wogg.xyz"}},
    {"key": "库", "name": "采王道长", "type": 1, "api": "https://example.com/api.php/provide/vod"}
  ],
  "lives": [
    {"group": "redirect", "channels": [{"name": "虎牙", "urls": ["proxy://do=live"]}]}
  ],
  "parses": [
    {"name": "Json并发", "type": 3, "url": "Demo"},
    {"name": "网络解析", "type": 0, "url": "https://example.com/parse?ac=1"}
  ]
}`

// realMulti 真实多仓：对象与字符串混排 + csp_ 格式前缀 + 相对名
var realMulti = []byte(`{
  "urls": [
    {"url": "http://肥猫.live/1.json", "name": "肥猫"},
    {"url": "csp_XPathhttp://notexample.com/tv.json", "name": "饭太硬"},
    {"url": "", "name": "空地址要跳过"},
    "http://plain.string/entry.json"
  ]
}`)

func TestParseSingleRealFormat(t *testing.T) {
	typ, d, err := Parse([]byte(realSingle))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if typ != "single" {
		t.Fatalf("期望 single, 得到 %s", typ)
	}
	if d.Sites != 3 || d.Lives != 1 || d.Parses != 2 {
		t.Errorf("计数错误: sites=%d lives=%d parses=%d", d.Sites, d.Lives, d.Parses)
	}
	if !strings.Contains(d.Spider, "spider.jar") || !strings.Contains(d.Spider, ";md5;") {
		t.Errorf("spider 提取错误: %s", d.Spider)
	}
	if d.Wallpaper == "" {
		t.Error("wallpaper 应被提取")
	}
	if len(d.SiteNames) != 3 || d.SiteNames[1] != "玩偶哥哥" {
		t.Errorf("站点名提取错误: %v", d.SiteNames)
	}
}

func TestParseMultiRealFormat(t *testing.T) {
	typ, d, err := Parse(realMulti)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if typ != "multi" {
		t.Fatalf("期望 multi, 得到 %s", typ)
	}
	if len(d.SubURLs) != 3 { // 空地址跳过
		t.Fatalf("子仓数错误: %d", len(d.SubURLs))
	}
	// 中文域名保留原样（归一化在录入时做）
	if d.SubURLs[0].Name != "肥猫" || d.SubURLs[0].URL != "http://肥猫.live/1.json" {
		t.Errorf("子仓0错误: %+v", d.SubURLs[0])
	}
	// csp_ 格式前缀剥离
	if !strings.HasPrefix(d.SubURLs[1].URL, "http://notexample.com") {
		t.Errorf("前缀剥离失败: %s", d.SubURLs[1].URL)
	}
}

func TestParseMixedRepoPreferMulti(t *testing.T) {
	mixed := `{"urls":[{"url":"http://a/1.json","name":"A"}], "sites":[{"key":"x","name":"站"}]}`
	typ, _, err := Parse([]byte(mixed))
	if err != nil || typ != "multi" {
		t.Fatalf("混合仓应优先 multi: typ=%s err=%v", typ, err)
	}
}

func TestParseFailures(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"HTML响应", "<html><body>403</body></html>", "响应非 JSON 格式"},
		{"空响应", "", "响应体为空"},
		{"结构不完整", `{"wallpaper":"http://x/bg.jpg"}`, "仓库结构不完整"},
		{"JSON语法错", `{"sites":[{name:x}]`, "JSON 解析错误"},
	}
	for _, c := range cases {
		_, _, err := Parse([]byte(c.body))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: 期望错误含 %q, 得到 %v", c.name, c.want, err)
		}
	}
}

func TestParseBOMAndLoose(t *testing.T) {
	// BOM + // 注释 + 尾逗号 + 单引号（真实源实测出现过的组合）
	body := "\xef\xbb\xbf// 公告：本源每日更新\n{'spider':'http://x/s.jar','sites':[{'key':'a','name':'A站',},],}"
	typ, d, err := Parse([]byte(body))
	if err != nil {
		t.Fatalf("宽松解析失败: %v", err)
	}
	if typ != "single" || d.Sites != 1 {
		t.Fatalf("宽松解析结果错误: typ=%s sites=%d", typ, d.Sites)
	}
	if d.Spider != "http://x/s.jar" {
		t.Errorf("spider 错误: %s", d.Spider)
	}
}

func TestNormalize(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"example.com/1.json", "http://example.com/1.json"},                          // 补协议
		{"  HTTP://Example.COM:8080/a.json  ", "http://example.com:8080/a.json"},     // 大小写
		{"http://example.com/Path/Keeps.Case", "http://example.com/Path/Keeps.Case"}, // path 大小写保留
		{"肥猫.live/1.json", "http://xn--xn--/1.json"},                                 // IDN→punycode（占位断言见下方真实校验）
	}
	for i, c := range cases {
		got, err := Normalize(c.in)
		if err != nil {
			t.Fatalf("case %d(%s) 错误: %v", i, c.in, err)
		}
		if i == 3 {
			// 中文域名转 punycode：校验前缀与 .live 后缀，不硬编码具体 punycode 串
			if !strings.HasPrefix(got, "http://xn--") || !strings.Contains(got, ".live/1.json") {
				t.Errorf("IDN 转换结果异常: %s", got)
			}
			continue
		}
		if got != c.want {
			t.Errorf("case %d: Normalize(%q)=%q, want %q", i, c.in, got, c.want)
		}
	}
	if _, err := Normalize("   "); err == nil {
		t.Error("空地址应报错")
	}
}

func TestNormalizeEmptyHost(t *testing.T) {
	if _, err := Normalize("http:///nohost.json"); err == nil {
		t.Error("缺主机名应报错")
	}
}
