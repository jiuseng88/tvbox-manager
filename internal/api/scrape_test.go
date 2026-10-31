package api

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"tvbox-manager/internal/model"
	"tvbox-manager/internal/store"
)

// extractScrapeEntries 是刮削同步的内容识别核心（纯函数，不联网），
// 覆盖：单仓 / 多仓 / 地址列表 JSON / txt 行列表 / 空内容。

func TestExtractScrapeEntriesSingle(t *testing.T) {
	text := `{"sites":[{"key":"a","name":"测试站","url":"https://api.example/api.php/provide/vod"}],"lives":[]}`
	entries, kind := extractScrapeEntries(text, "我的仓", "https://example.com/tvbox.json")
	if len(entries) != 1 {
		t.Fatalf("单仓应 1 条候选，实际 %d", len(entries))
	}
	if kind != "repo-single" {
		t.Fatalf("单仓 kind 应为 repo-single，实际 %s", kind)
	}
	if entries[0].URL != "https://example.com/tvbox.json" {
		t.Fatalf("单仓候选应为种子自身地址，实际 %s", entries[0].URL)
	}
	if entries[0].Name != "我的仓" {
		t.Fatalf("单仓候选名称应为种子名，实际 %s", entries[0].Name)
	}
}

func TestExtractScrapeEntriesMulti(t *testing.T) {
	text := `{"urls":[{"name":"子仓A","url":"https://a.example/sub1.json"},{"name":"子仓B","url":"https://b.example/sub2.json"}]}`
	entries, kind := extractScrapeEntries(text, "聚合", "https://agg.example/all.json")
	if kind != "repo-multi" {
		t.Fatalf("多仓 kind 应为 repo-multi，实际 %s", kind)
	}
	if len(entries) != 2 {
		t.Fatalf("多仓应 2 条候选，实际 %d", len(entries))
	}
	if entries[0].URL != "https://a.example/sub1.json" || entries[0].Name != "子仓A" {
		t.Fatalf("多仓子仓解析错误: %+v", entries[0])
	}
}

func TestExtractScrapeEntriesURLShareFormat(t *testing.T) {
	// TVBox 社区分享格式（带 // 注释的 JSONC）——urls 键存在即视为多仓子仓
	text := `// 分享列表
{"urls":[{"url":"https://x.example/a.json","name":"甲"},{"url":"https://y.example/b.json","name":"乙"}]}`
	entries, kind := extractScrapeEntries(text, "seed", "https://s.example/seed.json")
	if kind != "repo-multi" {
		t.Fatalf("分享格式应识别为 repo-multi，实际 %s", kind)
	}
	if len(entries) != 2 {
		t.Fatalf("应 2 条候选，实际 %d", len(entries))
	}
}

func TestExtractScrapeEntriesURLArray(t *testing.T) {
	// 纯字符串数组（无注释）
	text := `["https://x.example/a.json", "https://y.example/b.json"]`
	entries, kind := extractScrapeEntries(text, "seed", "s")
	if kind != "array" {
		t.Fatalf("数组 kind 应为 array，实际 %s", kind)
	}
	if len(entries) != 2 {
		t.Fatalf("应 2 条候选，实际 %d", len(entries))
	}
}

func TestExtractScrapeEntriesTxt(t *testing.T) {
	text := "测试源1,https://a.example/1.json\nhttps://b.example/2.json|源2\n# 注释行\n\nhttps://a.example/1.json\n"
	entries, kind := extractScrapeEntries(text, "seed", "x")
	if kind != "txt" {
		t.Fatalf("txt kind 应为 txt，实际 %s", kind)
	}
	if len(entries) != 2 {
		t.Fatalf("应 2 条候选（含注释与重复过滤），实际 %d", len(entries))
	}
}

func TestExtractScrapeEntriesEmpty(t *testing.T) {
	if entries, _ := extractScrapeEntries("   ", "s", "u"); len(entries) != 0 {
		t.Fatalf("空白内容应 0 条，实际 %d", len(entries))
	}
	if entries, _ := extractScrapeEntries("hello world 没有任何地址", "s", "u"); len(entries) != 0 {
		t.Fatalf("无地址内容应 0 条，实际 %d", len(entries))
	}
}

// TestScrapeApplyFlow 候选确认 → 导入为源的端到端闭环（不依赖网络：
// 候选直接注入 store，apply 走标准导入 + 状态回写）。
func TestScrapeApplyFlow(t *testing.T) {
	e := newEnv(t)

	// 1. 添加种子
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

	// 2. 注入候选：一条新地址 + 一条已存在（dup）的
	now := time.Now()
	c1 := model.ScrapeCandidate{ID: store.NewCandidateID(), SeedID: seedID, SeedName: "聚合源", Name: "新站1", URL: "https://new1.example/api.php", Kind: "repo-multi", Status: "pending", FetchedAt: now}
	c2 := model.ScrapeCandidate{ID: store.NewCandidateID(), SeedID: seedID, SeedName: "聚合源", Name: "重复站", URL: "https://dup.example/api.php", Kind: "repo-multi", Dup: true, Status: "pending", FetchedAt: now}
	if err := e.st.SetScrapeCandidates(seedID, []model.ScrapeCandidate{c1, c2}); err != nil {
		t.Fatal(err)
	}

	// 3. apply：非 dup 导入为源，dup 跳过
	code, body = e.doJSON("POST", "/api/scrape/apply", map[string]interface{}{
		"ids": []string{c1.ID, c2.ID}, "group": "刮削导入",
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

	// 4. 落库验证
	if _, found := e.st.GetByURL("https://new1.example/api.php"); !found {
		t.Fatal("新站应已入库")
	}
	if _, found := e.st.GetByURL("https://dup.example/api.php"); found {
		t.Fatal("重复站不应入库")
	}

	// 5. 候选状态回写
	st := map[string]string{}
	for _, c := range e.st.ListScrapeCandidates() {
		st[c.ID] = c.Status
	}
	if st[c1.ID] != "applied" {
		t.Fatalf("新站候选应为 applied，实际 %s", st[c1.ID])
	}
	if st[c2.ID] != "skipped" {
		t.Fatalf("重复站候选应为 skipped，实际 %s", st[c2.ID])
	}

	// 6. 二次 apply 同一候选不再新增（已处理过）
	code, body = e.doJSON("POST", "/api/scrape/apply", map[string]interface{}{"ids": []string{c1.ID}})
	if code != 200 {
		t.Fatalf("二次 apply 失败 %d: %v", code, body)
	}
	d = dataOf(t, body)
	if n, _ := d["applied"].(float64); n != 0 {
		t.Fatalf("二次 apply 不应新增，实际 %v", body)
	}
}

// ---- 直播源刮削（A-07/E-04）----

func TestExtractLiveSeedEntries(t *testing.T) {
	seedName, seedURL := "直播聚合", "https://live.agg.example/all.m3u"

	// 1. 整份 m3u → 种子自身一条候选（playlist）
	entries, kind := extractLiveSeedEntries("#EXTM3U\n#EXTINF:-1,央视综合\nhttp://x.example/cctv1.m3u8\n", seedName, seedURL)
	if len(entries) != 1 || kind != "playlist" || entries[0].URL != seedURL {
		t.Fatalf("m3u 应整份为 1 条 playlist 候选，实际 %d/%q/%v", len(entries), kind, entries)
	}

	// 2. txt 频道表（≥8 行 http）→ 整份
	var sb strings.Builder
	for i := 0; i < 10; i++ {
		sb.WriteString(fmt.Sprintf("频道%d,http://x.example/ch%d.m3u8\n", i, i))
	}
	entries, kind = extractLiveSeedEntries(sb.String(), seedName, seedURL)
	if len(entries) != 1 || kind != "playlist" || entries[0].URL != seedURL {
		t.Fatalf("txt 频道表应整份为 1 条 playlist，实际 %d/%q/%v", len(entries), kind, entries)
	}

	// 3. JSON lives 结构 → 提取播放列表直链（lives 键不过滤）
	entries, kind = extractLiveSeedEntries(`{"lives":[{"name":"A站","url":"http://a.example/x.m3u"},{"name":"B站","url":"http://b.example/y.txt"}]}`, seedName, seedURL)
	if len(entries) != 2 || kind != "live-json" {
		t.Fatalf("JSON lives 应提取 2 条，实际 %d/%q/%v", len(entries), kind, entries)
	}
	if entries[0].Name != "A站" {
		t.Fatalf("条目名称应为 A站，实际 %q", entries[0].Name)
	}

	// 4. JSON urls 混合，脚本/接口直链应被过滤
	entries, kind = extractLiveSeedEntries(`{"urls":[{"name":"脚本","url":"http://a.example/spider.js"},{"name":"播放","url":"http://c.example/live.m3u"},{"name":"CMS","url":"http://d.example/api.php/xx?y=1"}]}`, seedName, seedURL)
	if len(entries) != 1 || kind != "live-json" || entries[0].URL != "http://c.example/live.m3u" {
		t.Fatalf("应只收播放列表直链，实际 %d/%q/%v", len(entries), kind, entries)
	}

	// 5. txt 少量 URL 行 → 逐条候选（沿用批量导入口径：名称|地址 / 名称,地址 / 纯地址）
	entries, kind = extractLiveSeedEntries("A,http://a.example/x.m3u\nB|http://b.example/y.m3u\n", seedName, seedURL)
	if len(entries) != 2 || kind != "live-txt" {
		t.Fatalf("少量 URL 行应逐条 2 条，实际 %d/%q/%v", len(entries), kind, entries)
	}

	// 6. 空 / 纯文字 → 0 条
	if entries, _ := extractLiveSeedEntries("", seedName, seedURL); len(entries) != 0 {
		t.Fatalf("空内容应 0 条，实际 %d", len(entries))
	}
	if entries, _ := extractLiveSeedEntries("这个订阅页暂时没有内容", seedName, seedURL); len(entries) != 0 {
		t.Fatalf("纯文字应 0 条，实际 %d", len(entries))
	}
}

// TestScrapeApplyLiveFlow 直播候选 apply 端到端：候选 → LiveSource 入库 → 状态回写 → 幂等。
func TestScrapeApplyLiveFlow(t *testing.T) {
	e := newEnv(t)

	// 1. 添加 live 类型种子
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

	// 2. 注入 live 候选（URL 带 | 请求头参数，验证原样入库）
	now := time.Now()
	c := model.ScrapeCandidate{
		ID: store.NewCandidateID(), SeedID: seedID, SeedName: "直播聚合",
		Name: "海外综合", URL: "https://live.hw.example/iptv.m3u|User-Agent=okhttp/4.9.1",
		Target: "live", Kind: "live-json", Status: "pending", FetchedAt: now,
	}
	if err := e.st.SetScrapeCandidates(seedID, []model.ScrapeCandidate{c}); err != nil {
		t.Fatal(err)
	}

	// 3. apply → 应导入 1 条直播源
	code, body = e.doJSON("POST", "/api/scrape/apply", map[string]interface{}{"ids": []string{c.ID}, "group": "直播刮削"})
	if code != 200 {
		t.Fatalf("apply 失败 %d: %v", code, body)
	}
	d = dataOf(t, body)
	if n, _ := d["applied"].(float64); n != 1 {
		t.Fatalf("应导入 1 条直播源，实际 %v", body)
	}

	// 4. 落库验证（原样含 | 参数）
	found := false
	for _, ls := range e.st.AllLiveSources() {
		if ls.URL == c.URL {
			found = true
			if ls.Remark != "自动刮削导入" {
				t.Fatalf("直播源 remark 应为自动刮削导入，实际 %q", ls.Remark)
			}
		}
	}
	if !found {
		t.Fatal("候选直播源应已入库")
	}

	// 5. 候选状态回写
	st := map[string]string{}
	for _, cc := range e.st.ListScrapeCandidates() {
		st[cc.ID] = cc.Status
	}
	if st[c.ID] != "applied" {
		t.Fatalf("候选应为 applied，实际 %s", st[c.ID])
	}

	// 6. 二次 apply 同一候选 → 不再新增
	code, body = e.doJSON("POST", "/api/scrape/apply", map[string]interface{}{"ids": []string{c.ID}})
	if code != 200 {
		t.Fatalf("二次 apply 失败 %d: %v", code, body)
	}
	d = dataOf(t, body)
	if n, _ := d["applied"].(float64); n != 0 {
		t.Fatalf("二次 apply 不应新增，实际 %v", body)
	}
}
