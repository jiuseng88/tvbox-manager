package api

import (
	"path/filepath"
	"testing"
	"time"

	"tvbox-manager/internal/live"
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
