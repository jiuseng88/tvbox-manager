package api

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"tvbox-manager/internal/model"
)

// discoverContent 是刮削同步的内容识别核心（纯函数，不联网），覆盖：
// 单仓 / 多仓 / 地址列表 JSON / txt 行列表 / 空内容，以及逐条类型嗅探（source/live）。

func TestDiscoverScrapeSingle(t *testing.T) {
	text := `{"sites":[{"key":"a","name":"测试站","url":"https://api.example/api.php/provide/vod"}],"lives":[]}`
	items, err := discoverFn()(context.Background(), text, "我的仓", "https://example.com/tvbox.json", 1, 0, "", "", false, nil)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("单仓应 1 条候选，实际 %d", len(items))
	}
	if items[0].Kind != "repo-single" {
		t.Fatalf("单仓 kind 应为 repo-single，实际 %s", items[0].Kind)
	}
	if items[0].URL != "https://example.com/tvbox.json" {
		t.Fatalf("单仓候选应为种子自身地址，实际 %s", items[0].URL)
	}
	if items[0].Name != "我的仓" {
		t.Fatalf("单仓候选名称应为种子名，实际 %s", items[0].Name)
	}
	if items[0].Target != "source" {
		t.Fatalf("单仓候选应为 source，实际 %s", items[0].Target)
	}
}

func TestDiscoverScrapeMulti(t *testing.T) {
	text := `{"urls":[{"name":"子仓A","url":"https://a.example/sub1.json"},{"name":"子仓B","url":"https://b.example/sub2.json"}]}`
	items, err := discoverFn()(context.Background(), text, "聚合", "https://agg.example/all.json", 1, 0, "", "", false, nil)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("多仓应 2 条候选，实际 %d", len(items))
	}
	if items[0].Kind != "repo-multi" {
		t.Fatalf("多仓 kind 应为 repo-multi，实际 %s", items[0].Kind)
	}
	if items[0].URL != "https://a.example/sub1.json" || items[0].Name != "子仓A" {
		t.Fatalf("多仓子仓解析错误: %+v", items[0])
	}
}

func TestDiscoverScrapeURLShareFormat(t *testing.T) {
	text := `// 分享列表
{"urls":[{"url":"https://x.example/a.json","name":"甲"},{"url":"https://y.example/b.json","name":"乙"}]}`
	items, err := discoverFn()(context.Background(), text, "seed", "https://s.example/seed.json", 1, 0, "", "", false, nil)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("分享格式应 2 条候选，实际 %d", len(items))
	}
	for _, it := range items {
		if it.Target != "source" {
			t.Fatalf("分享格式应为 source，实际 %s", it.Target)
		}
	}
}

func TestDiscoverScrapeURLArray(t *testing.T) {
	text := `["https://x.example/a.json", "https://y.example/b.json"]`
	items, err := discoverFn()(context.Background(), text, "seed", "s", 1, 0, "", "", false, nil)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("数组应 2 条候选，实际 %d", len(items))
	}
	if items[0].Kind != "array" {
		t.Fatalf("数组 kind 应为 array，实际 %s", items[0].Kind)
	}
}

func TestDiscoverScrapeTxt(t *testing.T) {
	text := "测试源1,https://a.example/1.json\nhttps://b.example/2.json|源2\n# 注释行\n\nhttps://a.example/1.json\n"
	items, err := discoverFn()(context.Background(), text, "seed", "x", 1, 0, "", "", false, nil)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("应 2 条候选（含注释与重复过滤），实际 %d", len(items))
	}
	for _, it := range items {
		if it.Target != "source" {
			t.Fatalf("txt 源清单应为 source，实际 %s", it.Target)
		}
	}
}

func TestDiscoverScrapeEmpty(t *testing.T) {
	fn := discoverFn()
	if items, _ := fn(context.Background(), "   ", "s", "u", 1, 0, "", "", false, nil); len(items) != 0 {
		t.Fatalf("空白内容应 0 条，实际 %d", len(items))
	}
	if items, _ := fn(context.Background(), "hello world 没有任何地址", "s", "u", 1, 0, "", "", false, nil); len(items) != 0 {
		t.Fatalf("无地址内容应 0 条，实际 %d", len(items))
	}
}

// ---- 直播源发现（A-07 / E-04）----

func TestDiscoverLiveEntries(t *testing.T) {
	fn := discoverFn()
	seedName, seedURL := "直播聚合", "https://live.agg.example/all.m3u"

	// 1. 整份 m3u → 一条直播候选（playlist）
	items, err := fn(context.Background(), "#EXTM3U\n#EXTINF:-1,央视综合\nhttp://x.example/cctv1.m3u8\n", seedName, seedURL, 1, 0, "", "", true, nil)
	if err != nil || len(items) != 1 || items[0].Kind != "playlist" || items[0].Target != "live" || items[0].URL != seedURL {
		t.Fatalf("m3u 应整份为 1 条直播候选，实际 %d/%v/%v", len(items), items, err)
	}

	// 2. txt 频道表（≥8 行 http）→ 整份直播（playlist）
	var sb strings.Builder
	for i := 0; i < 10; i++ {
		sb.WriteString(fmt.Sprintf("频道%d,http://x.example/ch%d.m3u8\n", i, i))
	}
	items, err = fn(context.Background(), sb.String(), seedName, seedURL, 1, 0, "", "", true, nil)
	if err != nil || len(items) != 1 || items[0].Kind != "playlist" || items[0].Target != "live" {
		t.Fatalf("txt 频道表应整份为 1 条直播候选，实际 %d/%v/%v", len(items), items, err)
	}

	// 3. JSON lives 结构 → 提取播放列表直链（live-json）
	items, err = fn(context.Background(), `{"lives":[{"name":"A站","url":"http://a.example/x.m3u"},{"name":"B站","url":"http://b.example/y.txt"}]}`, seedName, seedURL, 1, 0, "", "", true, nil)
	if err != nil || len(items) != 2 || items[0].Target != "live" || items[0].Kind != "live-json" {
		t.Fatalf("JSON lives 应提取 2 条直播，实际 %d/%v/%v", len(items), items, err)
	}
	if items[0].Name != "A站" {
		t.Fatalf("条目名称应为 A站，实际 %q", items[0].Name)
	}

	// 4. JSON urls 混合，脚本/接口直链应被过滤，仅留播放列表直链
	items, err = fn(context.Background(), `{"urls":[{"name":"脚本","url":"http://a.example/spider.js"},{"name":"播放","url":"http://c.example/live.m3u"},{"name":"CMS","url":"http://d.example/api.php/xx?y=1"}]}`, seedName, seedURL, 1, 0, "", "", true, nil)
	if err != nil || len(items) != 1 || items[0].Target != "live" || items[0].URL != "http://c.example/live.m3u" {
		t.Fatalf("应只收播放列表直链（1 条直播），实际 %d/%v/%v", len(items), items, err)
	}

	// 5. txt 少量 URL 行 → 逐条直播候选（沿用批量导入口径：名称|地址 / 名称,地址 / 纯地址）
	items, err = fn(context.Background(), "A,http://a.example/x.m3u\nB|http://b.example/y.m3u\n", seedName, seedURL, 1, 0, "", "", true, nil)
	if err != nil || len(items) != 2 || items[0].Kind != "live-txt" || items[0].Target != "live" {
		t.Fatalf("少量 URL 行应逐条 2 条直播，实际 %d/%v/%v", len(items), items, err)
	}

	// 6. 空 / 纯文字 → 0 条
	if items, _ := fn(context.Background(), "", seedName, seedURL, 1, 0, "", "", true, nil); len(items) != 0 {
		t.Fatalf("空内容应 0 条，实际 %d", len(items))
	}
	if items, _ := fn(context.Background(), "这个订阅页暂时没有内容", seedName, seedURL, 1, 0, "", "", true, nil); len(items) != 0 {
		t.Fatalf("纯文字应 0 条，实际 %d", len(items))
	}
}

// discoverFn 取出真实的 discoverContent 方法（类型安全包装）。
func discoverFn() func(context.Context, string, string, string, int, int, string, string, bool, *hostLimiter) ([]discoveredItem, error) {
	return new(API).discoverContent
}

// TestScrapeApplyFlow 候选确认 → 导入为源的端到端闭环（不依赖网络：候选直接注入 store，apply 走标准导入 + 状态回写）。
func TestScrapeApplyFlow(t *testing.T) {
	e := newEnv(t)

	code, body := e.doJSON("POST", "/api/scrape/seeds", map[string]interface{}{
		"name": "聚合源", "url": "https://agg.example/all.json", "group": "刮削",
	})
	if code != 200 {
		t.Fatalf("添加种子失败 %d: %v", code, body)
	}
	d := dataOf(t, body)
	seedID, _ := d["id"].(string)
	if seedID == "" {
		t.Fatalf("种子 ID 为空: %v", d)
	}

	now := time.Now()
	c1 := model.ScrapeCandidate{SeedID: seedID, SeedName: "聚合源", Name: "新站1", URL: "https://new1.example/api.php", Kind: "repo-multi", Status: "pending", FetchedAt: now}
	c2 := model.ScrapeCandidate{SeedID: seedID, SeedName: "聚合源", Name: "重复站", URL: "https://dup.example/api.php", Kind: "repo-multi", Dup: true, Status: "pending", FetchedAt: now}
	if err := e.st.UpsertScrapeCandidates(seedID, []model.ScrapeCandidate{c1, c2}); err != nil {
		t.Fatal(err)
	}

	// 取出稳定 ID
	var id1, id2 string
	for _, c := range e.st.ListScrapeCandidates() {
		if c.URL == "https://new1.example/api.php" {
			id1 = c.ID
		}
		if c.URL == "https://dup.example/api.php" {
			id2 = c.ID
		}
	}

	code, body = e.doJSON("POST", "/api/scrape/apply", map[string]interface{}{
		"ids": []string{id1, id2}, "group": "刮削导入",
	})
	if code != 200 {
		t.Fatalf("apply 失败 %d: %v", code, body)
	}
	d = dataOf(t, body)
	if n, _ := d["applied"].(float64); n != 1 {
		t.Fatalf("应导入 1 条，实际 %v", body)
	}
	if n, _ := d["skipped"].(float64); n != 1 {
		t.Fatalf("应跳过 1 条（dup），实际 %v", body)
	}

	if _, found := e.st.GetByURL("https://new1.example/api.php"); !found {
		t.Fatal("新站应已入库")
	}
	if _, found := e.st.GetByURL("https://dup.example/api.php"); found {
		t.Fatal("重复站不应入库")
	}

	// 候选状态回写
	st := map[string]string{}
	for _, c := range e.st.ListScrapeCandidates() {
		st[c.ID] = c.Status
	}
	if st[id1] != "applied" {
		t.Fatalf("新站候选应为 applied，实际 %s", st[id1])
	}
	if st[id2] != "skipped" {
		t.Fatalf("重复站候选应为 skipped，实际 %s", st[id2])
	}

	// 二次 apply 同一候选不再新增（已处理过）
	code, body = e.doJSON("POST", "/api/scrape/apply", map[string]interface{}{"ids": []string{id1}})
	if code != 200 {
		t.Fatalf("二次 apply 失败 %d: %v", code, body)
	}
	d = dataOf(t, body)
	if n, _ := d["applied"].(float64); n != 0 {
		t.Fatalf("二次 apply 不应新增，实际 %v", body)
	}
}

// TestScrapeApplyLiveFlow 直播候选 apply 端到端：候选 → LiveSource 入库 → 状态回写 → 幂等。
func TestScrapeApplyLiveFlow(t *testing.T) {
	e := newEnv(t)

	code, body := e.doJSON("POST", "/api/scrape/seeds", map[string]interface{}{
		"name": "直播聚合", "url": "https://live.agg.example/all.m3u", "type": "live", "group": "直播刮削",
	})
	if code != 200 {
		t.Fatalf("添加 live 种子失败 %d: %v", code, body)
	}
	d := dataOf(t, body)
	seedID, _ := d["id"].(string)
	if seedID == "" {
		t.Fatalf("种子 ID 为空: %v", d)
	}

	now := time.Now()
	c := model.ScrapeCandidate{
		SeedID: seedID, SeedName: "直播聚合",
		Name: "海外综合", URL: "https://live.hw.example/iptv.m3u|User-Agent=okhttp/4.9.1",
		Target: "live", Kind: "live-json", Status: "pending", FetchedAt: now,
	}
	if err := e.st.UpsertScrapeCandidates(seedID, []model.ScrapeCandidate{c}); err != nil {
		t.Fatal(err)
	}
	var cid string
	for _, cc := range e.st.ListScrapeCandidates() {
		if cc.URL == c.URL {
			cid = cc.ID
		}
	}

	code, body = e.doJSON("POST", "/api/scrape/apply", map[string]interface{}{"ids": []string{cid}, "group": "直播刮削"})
	if code != 200 {
		t.Fatalf("apply 失败 %d: %v", code, body)
	}
	d = dataOf(t, body)
	if n, _ := d["applied"].(float64); n != 1 {
		t.Fatalf("应导入 1 条直播源，实际 %v", body)
	}

	found := false
	for _, ls := range e.st.AllLiveSources() {
		if ls.URL == c.URL {
			found = true
			if ls.Remark != "自动刮削导入" {
				t.Fatalf("直播源 remark 应为自动刮削导入，实际 %q", ls.Remark)
			}
			if ls.SeedID != seedID {
				t.Fatalf("直播源应带溯源 SeedID，实际 %q", ls.SeedID)
			}
		}
	}
	if !found {
		t.Fatal("候选直播源应已入库")
	}

	st := map[string]string{}
	for _, cc := range e.st.ListScrapeCandidates() {
		st[cc.ID] = cc.Status
	}
	if st[cid] != "applied" {
		t.Fatalf("候选应为 applied，实际 %s", st[cid])
	}

	// 二次 apply 同一候选 → 不再新增
	code, body = e.doJSON("POST", "/api/scrape/apply", map[string]interface{}{"ids": []string{cid}})
	if code != 200 {
		t.Fatalf("二次 apply 失败 %d: %v", code, body)
	}
	d = dataOf(t, body)
	if n, _ := d["applied"].(float64); n != 0 {
		t.Fatalf("二次 apply 不应新增，实际 %v", body)
	}
}

// TestScrapeApplyBatchFlush 验证批量导入只触发一次 flush（新增 AddManySources 路径）。
func TestScrapeApplyBatchFlush(t *testing.T) {
	e := newEnv(t)
	code, body := e.doJSON("POST", "/api/scrape/seeds", map[string]interface{}{
		"name": "批量", "url": "https://batch.example/all.json", "group": "刮削",
	})
	if code != 200 {
		t.Fatalf("添加种子失败 %d: %v", code, body)
	}
	seedID, _ := dataOf(t, body)["id"].(string)

	now := time.Now()
	var cands []model.ScrapeCandidate
	for i := 0; i < 5; i++ {
		cands = append(cands, model.ScrapeCandidate{
			SeedID: seedID, Name: fmt.Sprintf("src%d", i),
			URL: fmt.Sprintf("https://batch%d.example/api.php", i), Kind: "repo-multi", Status: "pending", FetchedAt: now,
		})
	}
	if err := e.st.UpsertScrapeCandidates(seedID, cands); err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(cands))
	for _, c := range e.st.ListScrapeCandidates() {
		ids = append(ids, c.ID)
	}
	code, body = e.doJSON("POST", "/api/scrape/apply", map[string]interface{}{"ids": ids, "group": "刮削导入"})
	if code != 200 {
		t.Fatalf("apply 失败 %d: %v", code, body)
	}
	d := dataOf(t, body)
	if n, _ := d["applied"].(float64); n != 5 {
		t.Fatalf("应导入 5 条，实际 %v", body)
	}
}

// TestParseGitHubRepo 覆盖包裹式代理前缀、.git 后缀、blob/tree 路径、非 GitHub 域名等识别边界。
func TestParseGitRepo(t *testing.T) {
	cases := []struct {
		in    string
		host  string
		owner string
		repo  string
		proxy string
		ok    bool
	}{
		{"https://gh-proxy.org/https://github.com/best-fan/iptv-sources.git", "github.com", "best-fan", "iptv-sources", "https://gh-proxy.org/", true},
		{"https://github.com/best-fan/iptv-sources", "github.com", "best-fan", "iptv-sources", "", true},
		{"https://github.com/best-fan/iptv-sources.git", "github.com", "best-fan", "iptv-sources", "", true},
		{"https://gitlab.com/group/iptv", "gitlab.com", "group", "iptv", "", true},
		{"https://gitee.com/mirrors/iptv.git", "gitee.com", "mirrors", "iptv", "", true},
		{"https://github.com/best-fan/iptv-sources/blob/main/x.m3u", "", "", "", "", false},
		{"https://github.com/best-fan/iptv-sources/tree/main/dir", "", "", "", "", false},
		{"https://raw.githubusercontent.com/best-fan/iptv-sources/main/x.m3u", "", "", "", "", false},
		{"https://example.com/foo.json", "", "", "", "", false},
		{"https://bitbucket.org/a/b", "", "", "", "", false}, // 不支持的主机
	}
	for _, c := range cases {
		host, owner, repo, proxy, ok := parseGitRepo(c.in)
		if ok != c.ok {
			t.Fatalf("parseGitRepo(%q) ok=%v 期望 %v", c.in, ok, c.ok)
		}
		if ok {
			if host != c.host || owner != c.owner || repo != c.repo || proxy != c.proxy {
				t.Fatalf("parseGitRepo(%q) = (%q,%q,%q,%q) 期望 (%q,%q,%q,%q)", c.in, host, owner, repo, proxy, c.host, c.owner, c.repo, c.proxy)
			}
		}
	}
}

// TestWalkRepoZip 用内存归档验证：解压后逐文件识别，且候选 URL 直接指向 raw.githubusercontent 直链。
func TestWalkRepoZip(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	write := func(name, content string) {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	write("iptv-sources-main/iptv.m3u", "#EXTM3U\n#EXTINF:-1 tvg-name=\"CCTV1\",CCTV1\nhttp://example.com/cctv1.m3u8\n")
	write("iptv-sources-main/tvbox.json", `{"sites":[{"key":"a","name":"测试站","url":"https://api.example/api.php/provide/vod"}],"lives":[]}`)
	// 应被忽略的非媒体文件
	write("iptv-sources-main/README.md", "# readme")
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	items, err := new(API).walkRepoZip(context.Background(), buf.Bytes(), "github.com", "best-fan", "iptv-sources", "https://gh-proxy.org/", "main", "种子", 1, 0, "", false, nil)
	if err != nil {
		t.Fatalf("walkRepoZip 不应报错: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("应识别出 2 条（m3u + json），实际 %d: %v", len(items), items)
	}
	var live, src *discoveredItem
	for i := range items {
		if items[i].Target == "live" {
			live = &items[i]
		} else if items[i].Target == "source" {
			src = &items[i]
		}
	}
	if live == nil || live.Kind != "playlist" {
		t.Fatalf("缺少直播播放列表候选: %v", items)
	}
	wantLive := "https://gh-proxy.org/https://raw.githubusercontent.com/best-fan/iptv-sources/main/iptv.m3u"
	if live.URL != wantLive {
		t.Fatalf("直播候选 URL 应为 raw 直链，实际 %s", live.URL)
	}
	if live.Depth != 1 {
		t.Fatalf("仓库内文件深度应为 1，实际 %d", live.Depth)
	}
	if src == nil || src.Kind != "repo-single" {
		t.Fatalf("缺少单仓源候选: %v", items)
	}
	wantSrc := "https://gh-proxy.org/https://raw.githubusercontent.com/best-fan/iptv-sources/main/tvbox.json"
	if src.URL != wantSrc {
		t.Fatalf("源候选 URL 应为 raw 直链，实际 %s", src.URL)
	}

	// GitLab：归档根目录 repo-branch/，raw 直链 gitlab.com/owner/repo/raw/branch/rel
	var buf2 bytes.Buffer
	zw2 := zip.NewWriter(&buf2)
	gw, _ := zw2.Create("iptv-main/channels.m3u")
	gw.Write([]byte("#EXTM3U\nhttp://e/c.m3u8\n"))
	zw2.Close()
	items2, err := new(API).walkRepoZip(context.Background(), buf2.Bytes(), "gitlab.com", "g", "iptv", "", "main", "gitlab种子", 1, 0, "", false, nil)
	if err != nil {
		t.Fatalf("gitlab walkRepoZip: %v", err)
	}
	if len(items2) != 1 {
		t.Fatalf("gitlab 应 1 条，实际 %d", len(items2))
	}
	if items2[0].URL != "https://gitlab.com/g/iptv/raw/main/channels.m3u" {
		t.Fatalf("gitlab raw URL 错误: %s", items2[0].URL)
	}

	// Gitee：归档根目录为 repo/（不带分支），raw 直链 gitee.com/owner/repo/raw/branch/rel
	var buf3 bytes.Buffer
	zw3 := zip.NewWriter(&buf3)
	ew, _ := zw3.Create("iptv/channels.m3u")
	ew.Write([]byte("#EXTM3U\nhttp://e/c.m3u8\n"))
	zw3.Close()
	items3, err := new(API).walkRepoZip(context.Background(), buf3.Bytes(), "gitee.com", "g", "iptv", "", "master", "gitee种子", 1, 0, "", false, nil)
	if err != nil {
		t.Fatalf("gitee walkRepoZip: %v", err)
	}
	if len(items3) != 1 {
		t.Fatalf("gitee 应 1 条，实际 %d", len(items3))
	}
	if items3[0].URL != "https://gitee.com/g/iptv/raw/master/channels.m3u" {
		t.Fatalf("gitee raw URL 错误: %s", items3[0].URL)
	}
}

// TestGitCloneRepoRejectsPrivateHost SSH/SSRF 防护：git 经自身出网，故 clone 前必须拦掉内网主机，
// 绝不能真的去连接。该测试离线、不依赖网络与 git 二进制。
func TestGitCloneRepoRejectsPrivateHost(t *testing.T) {
	cases := []struct {
		host, proxy string
	}{
		{"192.168.0.1", ""},     // 内网仓库主机
		{"10.0.0.5", ""},        // 内网仓库主机
		{"github.com", "http://127.0.0.1:8080/"}, // 公网仓库 + 内网包裹式代理
	}
	for _, c := range cases {
		_, _, err := new(API).gitCloneRepo(context.Background(), c.host, "o", "r", c.proxy)
		if err == nil {
			t.Fatalf("host=%s proxy=%s 应被拒绝", c.host, c.proxy)
		}
		if strings.Contains(err.Error(), "git clone 失败") {
			t.Fatalf("内网地址不应走到 git clone，但报错: %v", err)
		}
	}
}

// TestGitCloneRepoNoGitBinary 运行环境无 git 时，应明确报错交由归档兜底（不连网）。
func TestGitCloneRepoNoGitBinary(t *testing.T) {
	// 用 PATH 清空 + 仅含不存在目录，使 LookPath("git") 失败
	t.Setenv("PATH", "")
	_, _, err := new(API).gitCloneRepo(context.Background(), "github.com", "o", "r", "")
	if err == nil {
		t.Fatal("无 git 时应报错")
	}
	if strings.Contains(err.Error(), "git clone 失败") {
		t.Fatalf("无 git 二进制不应走到 git clone，但报错: %v", err)
	}
}
