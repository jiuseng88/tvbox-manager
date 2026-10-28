package parser

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"tvbox-manager/internal/model"
)

// zeroWidth 零宽字符与不可见空白
const zeroWidth = "\u200b\u200c\u200d\u2060\ufeff\u00a0"

// Normalize 归一化用户输入的地址：去空白/零宽字符、补协议、IDN 转 Punycode、
// scheme 与 host 转小写（path/query 保持原样，部分源站区分大小写）。
// 不做尾斜杠增删，保留用户原始输入形态。
func Normalize(raw string) (string, error) {
	s := strings.Trim(raw, zeroWidth)
	s = strings.TrimSpace(s)
	if s == "" {
		return "", errors.New("地址为空")
	}
	low := strings.ToLower(s)
	if !strings.HasPrefix(low, "http://") && !strings.HasPrefix(low, "https://") {
		s = "http://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", err
	}
	if u.Host == "" {
		return "", errors.New("缺少主机名")
	}
	u.Scheme = strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Host)
	// 中文国际域名 → Punycode；转换失败保留原样（部分环境可直接解析）。
	// 只有「:纯数字」结尾才算端口——IPv6 字面量（如 [::1]）本身就带冒号，
	// 按最后一个冒号硬切会把 host 切成 "[:" 这种非法值。
	if i := strings.LastIndex(host, ":"); i > 0 && isPortSuffix(host[i+1:]) {
		if ascii, err := idnaToASCII(host[:i]); err == nil {
			host = ascii + host[i:]
		}
	} else if ascii, err := idnaToASCII(host); err == nil {
		host = ascii
	}
	u.Host = host
	return u.String(), nil
}

// DedupKey 订阅去重用的键。Normalize 只统一了「写法」层面的差异（空白、协议、
// 主机大小写），但同一个地址仍可能有多种并存形态，实测最常见的两种：
//
//	https://a.com/tv   与   https://a.com/tv/     ← 多一个尾斜杠
//	http://a.com/x?b=2&a=1 与 ?a=1&b=2            ← 参数顺序不同
//
// 它们在客户端眼里是同一条线路，却会被当成两个站点源存下来，订阅里就重复了。
// 这里在 Normalize 基础上再抹平这些等价差异：
//   - 去掉 fragment（客户端不使用 # 之后的部分）
//   - 去掉默认端口（http:80 / https:443）
//   - 去掉末尾多余的斜杠（根路径也不例外）
//   - query 参数按键名排序
//
// 刻意不统一路径大小写：多数源站区分大小写，强行合并会把两条真线路并成一条。
// 归一化失败时退回去空白的原始串，保证调用方总能拿到可比较的键。
func DedupKey(raw string) string {
	norm, err := Normalize(raw)
	if err != nil {
		return strings.TrimSpace(raw)
	}
	u, err := url.Parse(norm)
	if err != nil {
		return norm
	}
	u.Fragment = ""
	u.RawFragment = ""
	if (u.Scheme == "http" && u.Port() == "80") || (u.Scheme == "https" && u.Port() == "443") {
		u.Host = u.Hostname()
	}
	if strings.HasSuffix(u.Path, "/") {
		u.Path = strings.TrimRight(u.Path, "/")
		u.RawPath = ""
	}
	if u.RawQuery != "" {
		u.RawQuery = u.Query().Encode() // Encode 会按键名排序
	}
	return u.String()
}

// isPortSuffix 判断冒号后是否为合法端口号（纯数字）
func isPortSuffix(s string) bool {
	if s == "" || len(s) > 5 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// HostOf 提取主机名（显示用兜底名称）
func HostOf(rawURL string) string {
	if u, err := url.Parse(rawURL); err == nil && u.Host != "" {
		return u.Host
	}
	return rawURL
}

type rawRepo struct {
	URLs      []json.RawMessage `json:"urls"`
	Sites     []json.RawMessage `json:"sites"`
	Lives     []json.RawMessage `json:"lives"`
	Parses    []json.RawMessage `json:"parses"`
	Spider    json.RawMessage   `json:"spider"`
	Wallpaper string            `json:"wallpaper"`
}

// Parse 识别仓库类型并提取摘要。
// 判定顺序：去 BOM/空白 → 必须以 '{' 开头 → 严格 JSON 解析 → 失败后宽松修复重试 →
// urls 非空数组 → multi（优先）；sites/lives/parses 任一非空 → single；否则结构不完整。
func Parse(body []byte) (string, *model.Detail, error) {
	body = bytes.TrimPrefix(body, []byte{0xEF, 0xBB, 0xBF}) // 去 UTF-8 BOM
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return model.TypeUnknown, nil, errors.New("响应体为空")
	}
	if body[0] != '{' {
		// HTML 直接判非 JSON；其余（BOM/注释/公告文字在前）先尝试宽松截取首个 '{'
		if body[0] == '<' {
			return model.TypeUnknown, nil, errors.New("响应非 JSON 格式")
		}
		fixed := bytes.TrimSpace(CleanJSON(body))
		if len(fixed) == 0 || fixed[0] != '{' {
			return model.TypeUnknown, nil, errors.New("响应非 JSON 格式")
		}
		body = fixed
	}

	var raw rawRepo
	if err := json.Unmarshal(body, &raw); err != nil {
		// 宽松修复（//注释、尾逗号、单引号字符串、前导杂文字）后重试
		fixed := CleanJSON(body)
		if bytes.Equal(bytes.TrimSpace(fixed), bytes.TrimSpace(body)) {
			return model.TypeUnknown, nil, fmt.Errorf("JSON 解析错误: %v", err)
		}
		if err2 := json.Unmarshal(fixed, &raw); err2 != nil {
			return model.TypeUnknown, nil, fmt.Errorf("JSON 解析错误: %v", err2)
		}
	}

	d := &model.Detail{RawSize: len(body), Wallpaper: raw.Wallpaper}
	d.Sites = len(raw.Sites)
	d.Lives = len(raw.Lives)
	d.Parses = len(raw.Parses)
	d.Spider = decodeStringish(raw.Spider)
	d.SiteNames = extractNames(raw.Sites, 50)
	d.LiveNames = extractNames(raw.Lives, 20)

	// 多仓优先
	if subs := parseSubURLs(raw.URLs); len(subs) > 0 {
		d.SubURLs = subs
		return model.TypeMulti, d, nil
	}
	if d.Sites > 0 || d.Lives > 0 || d.Parses > 0 {
		return model.TypeSingle, d, nil
	}
	return model.TypeUnknown, nil, errors.New("仓库结构不完整（缺少 sites/lives/urls）")
}

// extractNames sites/lives 数组提取 name 字段
func extractNames(items []json.RawMessage, limit int) []string {
	names := make([]string, 0, len(items))
	for _, it := range items {
		var o struct {
			Name  string `json:"name"`
			Group string `json:"group"`
		}
		if json.Unmarshal(it, &o) != nil {
			continue
		}
		n := o.Name
		if n == "" {
			n = o.Group
		}
		if n != "" && len(names) < limit {
			names = append(names, n)
		}
	}
	return names
}

// parseSubURLs 解析多仓 urls 数组，兼容对象与字符串两种写法；
// 名称/地址同时兼容 name/sourceName 与 url/sourceUrl 两套键（多仓配置两种写法都常见）；
// 剥离 "csp_XPathhttp://..." 类格式前缀。
func parseSubURLs(items []json.RawMessage) []model.SubURL {
	out := make([]model.SubURL, 0, len(items))
	for _, it := range items {
		var o struct {
			URL        string `json:"url"`
			SourceURL  string `json:"sourceUrl"`
			Name       string `json:"name"`
			SourceName string `json:"sourceName"`
		}
		if err := json.Unmarshal(it, &o); err != nil {
			var s string
			if json.Unmarshal(it, &s) == nil {
				o.URL = s
			} else {
				continue
			}
		}
		u := o.URL
		if u == "" {
			u = o.SourceURL
		}
		if u == "" {
			continue
		}
		u = stripFormatPrefix(u)
		if u == "" {
			continue
		}
		n := o.Name
		if n == "" {
			n = o.SourceName
		}
		out = append(out, model.SubURL{Name: n, URL: u})
	}
	return out
}

// stripFormatPrefix 剥离 "csp_XPathhttp://xxx"、"jsonhttp://xxx" 类前缀：
// 定位第一个 http:// 或 https://，之前的全部丢弃。
func stripFormatPrefix(u string) string {
	if strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") {
		return u
	}
	if i := strings.Index(u, "http://"); i > 0 {
		return u[i:]
	}
	if i := strings.Index(u, "https://"); i > 0 {
		return u[i:]
	}
	return u
}

// decodeStringish spider 可能是字符串或对象（{url:...}/{jar:...}）
func decodeStringish(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	if raw[0] == '"' {
		var s string
		_ = json.Unmarshal(raw, &s)
		return s
	}
	if raw[0] == '{' {
		var o struct {
			URL string `json:"url"`
			Jar string `json:"jar"`
		}
		if json.Unmarshal(raw, &o) == nil {
			if o.URL != "" {
				return o.URL
			}
			return o.Jar
		}
	}
	return ""
}

// Summarize 成功探活的摘要文案
func Summarize(d *model.Detail, typ string) string {
	if typ == model.TypeMulti && d != nil && len(d.SubURLs) > 0 {
		return fmt.Sprintf("%d 个子仓", len(d.SubURLs))
	}
	if d == nil {
		return "ok"
	}
	msg := fmt.Sprintf("%d 站点 / %d 直播 / %d 解析", d.Sites, d.Lives, d.Parses)
	return msg
}
