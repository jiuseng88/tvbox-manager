package api

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"tvbox-manager/internal/live"
	"tvbox-manager/internal/store"
)

// 落盘缓存往返：频道表、来源名、时间戳都要原样保留，重启后才能真正秒开。
func TestLiveCacheFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "live_cache.json")
	now := time.Now().Truncate(time.Second)
	p := &tvLivePayload{
		At:       now,
		Channels: []live.Channel{{Group: "央视", Name: "CCTV-1", URLs: []string{"http://a/1.m3u8"}, Source: "源A"}},
		Groups:   []string{"央视"},
		Count:    1,
		Sources:  []tvLiveSourceStat{{Name: "源A", URL: "http://a/list.m3u", OK: true, Channels: 1}},
	}
	saveLiveCacheFile(path, p)

	got, ok := loadLiveCacheFile(path)
	if !ok {
		t.Fatal("loadLiveCacheFile 应成功")
	}
	if len(got.Channels) != 1 || got.Channels[0].Name != "CCTV-1" || got.Channels[0].Source != "源A" {
		t.Fatalf("频道往返错误: %+v", got.Channels)
	}
	if !got.At.Equal(now) {
		t.Fatalf("At 往返错误: %v != %v", got.At, now)
	}
	if len(got.Sources) != 1 || got.Sources[0].Name != "源A" {
		t.Fatalf("来源统计往返错误: %+v", got.Sources)
	}
}

// 不存在 / 空频道表的缓存都视为无效，避免把空页当降级数据下发。
func TestLiveCacheFileInvalid(t *testing.T) {
	if _, ok := loadLiveCacheFile(filepath.Join(t.TempDir(), "nope.json")); ok {
		t.Fatal("不存在文件应返回 false")
	}
	path := filepath.Join(t.TempDir(), "empty.json")
	saveLiveCacheFile(path, &tvLivePayload{At: time.Now()})
	if _, ok := loadLiveCacheFile(path); ok {
		t.Fatal("空频道表应视为无效缓存")
	}
}

// 重名直播源必须被唯一化：频道只带来源名，名字重复会让直播页下拉把它们并成
// 一项，筛选时不同源混作一坨（表现为「下拉的源数少于管理端的条目数」）。
func TestTVLiveSitesUniqueNames(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	add := func(name, u string) {
		if _, err := st.AddLiveSource(name, u, "默认", "", true, 10); err != nil {
			t.Fatalf("添加直播源 %q 失败: %v", name, err)
		}
	}
	add("B站直播", "http://a.example/1.m3u")
	add("B站直播", "http://b.example/2.m3u")
	add("B站直播", "http://c.example/3.m3u")
	add("央视", "http://d.example/4.m3u")

	a := &API{st: st}
	sites := a.tvLiveSites(context.Background())
	if len(sites) != 4 {
		t.Fatalf("应收集 4 个直播源，实际 %d: %+v", len(sites), sites)
	}
	want := []string{"B站直播", "B站直播 #2", "B站直播 #3", "央视"}
	seen := make(map[string]bool, len(sites))
	for i, s := range sites {
		if s.Name != want[i] {
			t.Fatalf("第 %d 个源名 = %q，期望 %q", i, s.Name, want[i])
		}
		if seen[s.Name] {
			t.Fatalf("源名重复: %q", s.Name)
		}
		seen[s.Name] = true
	}
}

// uniqueSourceName 与用户自己写的「xx #2」冲突时要继续避让，保证结果一定唯一。
func TestUniqueSourceName(t *testing.T) {
	used := map[string]bool{}
	cases := []struct{ in, want string }{
		{"B站直播", "B站直播"},
		{"B站直播", "B站直播 #2"},
		{"B站直播", "B站直播 #3"},
		{"B站直播 #2", "B站直播 #2 #2"},
		{"Guovin", "Guovin"},
	}
	for _, c := range cases {
		if got := uniqueSourceName(c.in, used); got != c.want {
			t.Fatalf("uniqueSourceName(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}

func TestFilterPlayable(t *testing.T) {
	in := []live.Channel{
		{Group: "央视", Name: "CCTV-1", URLs: []string{"http://a/1.m3u8"}},
		{Group: "央视", Name: "空线路", URLs: nil},
		{Group: "卫视", Name: "全空串", URLs: []string{"", "  "}},
		{Group: "卫视", Name: "湖南卫视", URLs: []string{"http://a/hn.m3u8", "http://b/hn.m3u8"}},
	}
	out := filterPlayable(in)
	if len(out) != 2 {
		t.Fatalf("应只保留 2 个有线路的频道，实际 %d: %+v", len(out), out)
	}
	if out[0].Name != "CCTV-1" || out[1].Name != "湖南卫视" {
		t.Fatalf("过滤结果或顺序不对: %+v", out)
	}
	// 原地过滤复用底层数组：多出来的槽位不该再被引用
	if len(filterPlayable(nil)) != 0 {
		t.Fatal("nil 输入应返回空")
	}
	if got := filterPlayable([]live.Channel{{Group: "x", Name: "y"}}); len(got) != 0 {
		t.Fatalf("全部无线路时应返回空，实际 %+v", got)
	}
}

