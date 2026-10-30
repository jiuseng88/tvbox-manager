// Package live 解析直播播放列表，把 m3u / txt / json 三种常见格式统一成 Channel。
//
// 这里只做「文本 → 频道」的纯函数变换，不发网络请求、不碰存储；
// 拉取与缓存由 api 层负责，便于单测直接喂样本。
package live

import (
	"bytes"
	"encoding/json"
	"net/url"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/transform"
)

// DefaultGroup 频道没有分组信息时归到这里，前端保证有个兜底 tab
const DefaultGroup = "未分组"

// Channel 一个直播频道。
// 同一分组下同名的多个地址视为「同一频道的多条线路」，合并进 URLs，前端据此换线。
type Channel struct {
	Group  string   `json:"group"`
	Name   string   `json:"name"`
	URLs   []string `json:"urls"`             // 原样保留（可能带 |User-Agent=... 参数）
	Logo   string   `json:"logo,omitempty"`   //
	Source string   `json:"source,omitempty"` // 来源直播源名，播放失败时用于定位
}

// SplitHeader 拆分地址里的 | 请求头参数：
// http://a/b.m3u8|User-Agent=Mozilla&Referer=http://a/  →  ("http://a/b.m3u8", "User-Agent=Mozilla&Referer=http://a/")
// 播放列表和频道地址里这种写法非常普遍，不拆开就会把整串当 URL 去请求，必然 404。
func SplitHeader(raw string) (stem, header string) {
	if i := strings.Index(raw, "|"); i >= 0 {
		return strings.TrimSpace(raw[:i]), strings.TrimSpace(raw[i+1:])
	}
	return strings.TrimSpace(raw), ""
}

// Decode 把响应体转成文本：去 BOM；不是合法 UTF-8 时按 GB18030 解。
// 国内 txt 直播源大量是 GBK，直接当 UTF-8 用会让频道名变乱码、分组失效。
func Decode(body []byte) string {
	body = bytes.TrimPrefix(body, []byte{0xEF, 0xBB, 0xBF})
	if utf8.Valid(body) {
		return string(body)
	}
	if dec, _, err := transform.Bytes(simplifiedchinese.GB18030.NewDecoder(), body); err == nil {
		return string(dec)
	}
	return string(body)
}

// Parse 自动识别格式并解析。base 用于把相对地址补成绝对地址（可为 nil）。
func Parse(body []byte, base *url.URL, source string) []Channel {
	text := Decode(body)
	if text == "" {
		return nil
	}
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil
	}

	var out []Channel
	switch {
	case strings.HasPrefix(strings.ToUpper(trimmed), "#EXTM3U") || strings.Contains(trimmed, "#EXTINF"):
		out = parseM3U(trimmed, base, source)
	case trimmed[0] == '[' || trimmed[0] == '{':
		// JSON 解析不出来时退回按 txt 处理（部分源把 txt 命名成 .json）
		if js := parseJSON(trimmed, base, source); len(js) > 0 {
			out = js
		} else {
			out = parseTXT(trimmed, base, source)
		}
	default:
		out = parseTXT(trimmed, base, source)
	}
	return Merge(out)
}

// ---- M3U / M3U8 ----

// attrRe 从 #EXTINF 行里抠出 key="value" 属性（tvg-logo / group-title / tvg-name ...）
func m3uAttrs(line string) map[string]string {
	attrs := make(map[string]string, 6)
	i := 0
	for i < len(line) {
		eq := strings.IndexByte(line[i:], '=')
		if eq < 0 {
			break
		}
		eq += i
		keyStart := eq
		for keyStart > i && (isAlphaNum(line[keyStart-1]) || line[keyStart-1] == '-') {
			keyStart--
		}
		key := strings.ToLower(strings.TrimSpace(line[keyStart:eq]))
		pos := eq + 1
		for pos < len(line) && (line[pos] == ' ' || line[pos] == '\t') {
			pos++
		}
		if pos >= len(line) {
			break
		}
		var val string
		if line[pos] == '"' {
			if end := strings.IndexByte(line[pos+1:], '"'); end >= 0 {
				val = line[pos+1 : pos+1+end]
				pos = pos + end + 2
			} else {
				val = line[pos+1:]
				pos = len(line)
			}
		} else {
			end := pos
			for end < len(line) && line[end] != ' ' && line[end] != '\t' {
				end++
			}
			val = line[pos:end]
			pos = end
		}
		if key != "" {
			attrs[key] = val
		}
		i = pos
	}
	return attrs
}

// m3uTitle 取 #EXTINF 逗号之后的显示名。名称里本身可能有逗号，
// 但属性值里的逗号已被引号包住，所以从「最后一个引号之后」找第一个逗号最稳。
func m3uTitle(line string) string {
	start := 0
	if i := strings.LastIndexByte(line, '"'); i >= 0 {
		start = i + 1
	}
	if c := strings.IndexByte(line[start:], ','); c >= 0 {
		return strings.TrimSpace(line[start+c+1:])
	}
	return ""
}

func isAlphaNum(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9'
}

func parseM3U(text string, base *url.URL, source string) []Channel {
	var out []Channel
	var pendingName, pendingGroup, pendingLogo string
	curGroup := ""

	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			up := strings.ToUpper(line)
			switch {
			case strings.HasPrefix(up, "#EXTINF"):
				attrs := m3uAttrs(line)
				pendingName = m3uTitle(line)
				if pendingName == "" {
					pendingName = attrs["tvg-name"]
				}
				pendingLogo = attrs["tvg-logo"]
				if g := strings.TrimSpace(attrs["group-title"]); g != "" {
					pendingGroup = g
				} else {
					pendingGroup = curGroup
				}
			case strings.HasPrefix(up, "#EXTGRP:"):
				curGroup = strings.TrimSpace(line[len("#EXTGRP:"):])
				pendingGroup = curGroup
			}
			continue
		}

		u := resolveURL(line, base)
		if u == "" {
			continue
		}
		name := pendingName
		if name == "" {
			// 没有 #EXTINF 的媒体清单（直接贴一个 m3u8 地址）拿不到频道名，
			// 用源名称兜底，否则频道列表里会显示成一串主机名
			name = source
			if name == "" {
				name = guessNameFromURL(u)
			}
		}
		group := pendingGroup
		if group == "" {
			group = DefaultGroup
		}
		out = append(out, Channel{Group: group, Name: name, URLs: []string{u}, Logo: pendingLogo, Source: source})
		pendingName, pendingLogo = "", ""
	}
	return out
}

// ---- TXT ----

func parseTXT(text string, base *url.URL, source string) []Channel {
	var out []Channel
	group := DefaultGroup

	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") {
			continue
		}
		// 只按第一个逗号切：URL 的 query 里常带逗号，硬切会撕碎地址
		comma := strings.IndexByte(line, ',')
		if comma < 0 {
			continue
		}
		name := strings.TrimSpace(line[:comma])
		rest := strings.TrimSpace(line[comma+1:])
		if name == "" || rest == "" {
			continue
		}
		if strings.EqualFold(rest, "#genre#") || rest == "#genre#" {
			group = name
			continue
		}

		for _, one := range splitLines(rest) {
			u := resolveURL(one, base)
			if u == "" {
				continue
			}
			out = append(out, Channel{Group: group, Name: name, URLs: []string{u}, Source: source})
		}
	}
	return out
}

// splitLines 拆 TXT 里的多线路：常规写法 名称,url1#url2#url3。
// 只有以 http 开头的片段才算线路，避免把 URL 自带的 fragment 也切开。
func splitLines(rest string) []string {
	if !strings.Contains(rest, "#") {
		return []string{rest}
	}
	parts := strings.Split(rest, "#")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" && strings.HasPrefix(strings.ToLower(p), "http") {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return []string{rest}
	}
	return out
}

// ---- JSON ----

func parseJSON(text string, base *url.URL, source string) []Channel {
	type rawItem struct {
		Group    string          `json:"group"`
		Name     string          `json:"name"`
		URL      string          `json:"url"`
		URLs     []string        `json:"urls"`
		Logo     string          `json:"logo"`
		Channels json.RawMessage `json:"channels"`
	}

	var items []rawItem
	if err := json.Unmarshal([]byte(text), &items); err != nil {
		var wrap struct {
			Channels []rawItem `json:"channels"`
			Lives    []rawItem `json:"lives"`
		}
		if err := json.Unmarshal([]byte(text), &wrap); err != nil {
			return nil
		}
		items = append(wrap.Channels, wrap.Lives...)
	}

	var out []Channel
	for _, it := range items {
		group := strings.TrimSpace(it.Group)
		if group == "" {
			group = DefaultGroup
		}
		cands := make([]string, 0, len(it.URLs)+1)
		if it.URL != "" {
			cands = append(cands, it.URL)
		}
		cands = append(cands, it.URLs...)
		for _, c := range cands {
			u := resolveURL(c, base)
			if u == "" {
				continue
			}
			name := strings.TrimSpace(it.Name)
			if name == "" {
				name = source
				if name == "" {
					name = guessNameFromURL(u)
				}
			}
			out = append(out, Channel{Group: group, Name: name, URLs: []string{u}, Logo: it.Logo, Source: source})
		}
	}
	return out
}

// ---- 公共 ----

// resolveURL 补全相对地址并做基本过滤：只留 http/https；proxy:// 之类一律丢弃。
func resolveURL(raw string, base *url.URL) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	stem, header := SplitHeader(raw)
	low := strings.ToLower(stem)
	if !strings.HasPrefix(low, "http://") && !strings.HasPrefix(low, "https://") {
		if base == nil || strings.HasPrefix(low, "proxy://") || strings.Contains(low, "://") {
			return ""
		}
		ref, err := url.Parse(stem)
		if err != nil {
			return ""
		}
		stem = base.ResolveReference(ref).String()
		if !strings.HasPrefix(strings.ToLower(stem), "http") {
			return ""
		}
	}
	if header != "" {
		return stem + "|" + header
	}
	return stem
}

func guessNameFromURL(u string) string {
	if p, err := url.Parse(u); err == nil && p.Host != "" {
		return p.Host
	}
	return u
}

// Merge 合并同组同名频道：线路去重后拼在一起，logo 取第一个非空的。
// 多个直播源里大量频道重名（CCTV1 几乎每份列表都有），不合并会给前端刷出一屏重复卡片。
func Merge(in []Channel) []Channel {
	type key struct{ group, name string }
	idx := make(map[key]int, len(in))
	out := make([]Channel, 0, len(in))
	for _, ch := range in {
		if ch.Name == "" {
			continue
		}
		k := key{ch.Group, ch.Name}
		if i, ok := idx[k]; ok {
			seen := make(map[string]bool, len(out[i].URLs))
			for _, u := range out[i].URLs {
				seen[u] = true
			}
			for _, u := range ch.URLs {
				if u != "" && !seen[u] {
					out[i].URLs = append(out[i].URLs, u)
					seen[u] = true
				}
			}
			if out[i].Logo == "" {
				out[i].Logo = ch.Logo
			}
			continue
		}
		idx[k] = len(out)
		out = append(out, ch)
	}
	return out
}
