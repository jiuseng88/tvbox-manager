package store

import (
	"math"
	"sort"
	"strings"
	"time"

	"tvbox-manager/internal/model"
)

// ---------- 表头排序（sort_by + sort_dir） ----------
// 通用比较辅助：返回 -1/0/1，配合 sort.SliceStable 使用；
// 主键相同时由调用方回退到默认排序（Sort/Name/URL），保证跨页顺序稳定。

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func cmpInt64(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func cmpTimePtr(a, b *time.Time) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return 1 // 未检测排后面
	case b == nil:
		return -1
	case a.Before(*b):
		return -1
	case a.After(*b):
		return 1
	}
	return 0
}

func cmpBool(a, b bool) int {
	switch {
	case a == b:
		return 0
	case a:
		return -1 // 升序时启用在前
	}
	return 1
}

// latencyVal 仅 ok 状态有有效延迟，其余视为无穷大（升序时集中排后面）
func latencyVal(st string, ms int64) int64 {
	if st == model.StatusOK {
		return ms
	}
	return math.MaxInt64
}

// availRank 可用性分组：可用的排最前，其余（失效/未检测/检测中）一组排后面
func availRank(st string) int {
	if st == model.StatusOK {
		return 0
	}
	return 1
}

// cmpSourceAvail / cmpFlatAvail 列表与订阅共用的默认排序：
//
//	可用的在前 → 同组内延迟小的在前（非 ok 视为同组）→ 创建时间早的在前（保底）
//	之后再用 Sort/Name/URL 定序，保证跨请求顺序稳定、翻页不跳动。
//
// 这样后台列表看到的顺序 = 订阅接口下发给客户端的顺序，排障时不用两套脑子。
func cmpSourceAvail(a, b model.Source) int {
	if c := cmpInt(availRank(a.Status), availRank(b.Status)); c != 0 {
		return c
	}
	if c := cmpInt64(latencyVal(a.Status, a.LatencyMs), latencyVal(b.Status, b.LatencyMs)); c != 0 {
		return c
	}
	if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
		return c
	}
	if c := cmpInt(a.Sort, b.Sort); c != 0 {
		return c
	}
	if c := strings.Compare(a.Name, b.Name); c != 0 {
		return c
	}
	return strings.Compare(a.URL, b.URL)
}

func cmpFlatAvail(a, b model.FlatSource) int {
	if c := cmpInt(availRank(a.Status), availRank(b.Status)); c != 0 {
		return c
	}
	if c := cmpInt64(latencyVal(a.Status, a.LatencyMs), latencyVal(b.Status, b.LatencyMs)); c != 0 {
		return c
	}
	if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
		return c
	}
	if c := cmpInt(a.Sort, b.Sort); c != 0 {
		return c
	}
	if c := strings.Compare(a.Name, b.Name); c != 0 {
		return c
	}
	return strings.Compare(a.URL, b.URL)
}

// cmpSource 源列表表头排序键比较
func cmpSource(a, b model.Source, key string) int {
	switch key {
	case "status":
		return cmpInt(statusRank(a.Status), statusRank(b.Status))
	case "name":
		return strings.Compare(a.Name, b.Name)
	case "type":
		return strings.Compare(a.Type, b.Type)
	case "url":
		return strings.Compare(a.URL, b.URL)
	case "group":
		return strings.Compare(a.Group, b.Group)
	case "latency":
		return cmpInt64(latencyVal(a.Status, a.LatencyMs), latencyVal(b.Status, b.LatencyMs))
	case "checks":
		return cmpInt(a.CheckCount, b.CheckCount)
	case "fails":
		return cmpInt(a.FailCount, b.FailCount)
	case "time":
		return cmpTimePtr(a.LastCheckAt, b.LastCheckAt)
	case "enabled":
		return cmpBool(a.Enabled, b.Enabled)
	case "sort":
		return cmpInt(a.Sort, b.Sort)
	}
	return 0
}

// cmpFlat 所有仓源表头排序键比较（启用按生效状态）
func cmpFlat(a, b model.FlatSource, key string) int {
	switch key {
	case "status":
		return cmpInt(statusRank(a.Status), statusRank(b.Status))
	case "name":
		return strings.Compare(a.Name, b.Name)
	case "url":
		return strings.Compare(a.URL, b.URL)
	case "group":
		return strings.Compare(a.Group, b.Group)
	case "latency":
		return cmpInt64(latencyVal(a.Status, a.LatencyMs), latencyVal(b.Status, b.LatencyMs))
	case "checks":
		return cmpInt(a.CheckCount, b.CheckCount)
	case "fails":
		return cmpInt(a.FailCount, b.FailCount)
	case "time":
		return cmpTimePtr(a.LastCheckAt, b.LastCheckAt)
	case "enabled":
		return cmpBool(a.EffectiveEnabled, b.EffectiveEnabled)
	case "sort":
		return cmpInt(a.Sort, b.Sort)
	}
	return 0
}

// sortByHeader 按表头键排序，主键相同回退默认序（Sort/Name/URL）
func sortByHeader[T any](out []T, less func(i, j T) int, desc bool, fallback func(i, j T) bool) {
	sort.SliceStable(out, func(i, j int) bool {
		c := less(out[i], out[j])
		if desc {
			c = -c
		}
		if c != 0 {
			return c < 0
		}
		return fallback(out[i], out[j])
	})
}

func sourceFallback(a, b model.Source) bool {
	if a.Sort != b.Sort {
		return a.Sort < b.Sort
	}
	if a.Name != b.Name {
		return a.Name < b.Name
	}
	return a.URL < b.URL // 兜底定序：保证跨请求顺序稳定，翻页不跳动
}

func flatFallback(a, b model.FlatSource) bool {
	if a.Sort != b.Sort {
		return a.Sort < b.Sort
	}
	if a.Name != b.Name {
		return a.Name < b.Name
	}
	return a.URL < b.URL
}
