package checker

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"tvbox-manager/internal/model"
	"tvbox-manager/internal/parser"
)

const maxBody = 20 << 20 // 20MB：大仓库 JSON（几千站点/上百子仓）10MB 装不下，会被误判成失效

// maxTimeout 单次探活超时的硬上限（秒）。client 级超时固定用它兜底，
// 单条源单独配置的超时（无论大小）都由每次请求的 context 精确控制。
const maxTimeout = 120 * time.Second

// defaultUA 探活固定 UA：大量源站对浏览器 UA 返回 403，必须模拟 TVBox 客户端，不开放配置
const defaultUA = "okhttp/4.9.1"

// Checker 单源探活与实时预览共用引擎
type Checker struct {
	client  *http.Client
	timeout time.Duration
}

func New(cfg *model.Config) *Checker {
	timeout := time.Duration(cfg.CheckTimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	tr := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     30 * time.Second,
		TLSClientConfig:     &tls.Config{InsecureSkipVerify: cfg.InsecureSkipVerify},
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 15 * time.Second,
		}).DialContext,
	}
	return &Checker{
		client: &http.Client{
			Transport: tr,
			// 兜底上限，不是日常超时：日常超时由每次请求的 context 控制，
			// 这样单条源才能配置出比全局更长的超时（大仓库拉取慢）。
			Timeout: maxTimeout,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 3 {
					return fmt.Errorf("stopped after 3 redirects")
				}
				return nil
			},
		},
		timeout: timeout,
	}
}

// TimeoutSeconds 超时秒数（错误文案用）
func (c *Checker) TimeoutSeconds() int {
	return int(c.timeout / time.Second)
}

// Result 探活/预览结果
type Result struct {
	Status    string
	HTTPCode  int
	LatencyMs int64
	Msg       string
	Type      string
	Detail    *model.Detail
	Size      int
	CheckedAt time.Time
	FinalURL  string
}

// Fetch 拉取并解析。guardPrivate=true 时拒绝内网/链路本地地址（SSRF 防护）。
// Fetch 探活一个地址，超时用全局配置
func (c *Checker) Fetch(ctx context.Context, rawURL string, guardPrivate bool) (*Result, []byte) {
	return c.FetchT(ctx, rawURL, guardPrivate, 0)
}

// FetchT 探活一个地址。timeoutSeconds>0 时覆盖全局超时（单条源单独配置用，
// 允许比全局更长——大仓库拉取慢）；<=0 用全局。范围 1-120 秒，超出按全局处理。
func (c *Checker) FetchT(ctx context.Context, rawURL string, guardPrivate bool, timeoutSeconds int) (*Result, []byte) {
	u, err := parser.Normalize(rawURL)
	if err != nil {
		return failResult(0, 0, "地址格式非法: "+err.Error()), nil
	}
	if guardPrivate {
		if err := RejectPrivateHost(hostOf(u)); err != nil {
			return failResult(0, 0, "地址被拒绝: "+err.Error()), nil
		}
	}

	timeout := c.timeout
	if timeoutSeconds > 0 && timeoutSeconds <= 120 {
		timeout = time.Duration(timeoutSeconds) * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return failResult(0, 0, "构造请求失败: "+err.Error()), nil
	}
	req.Header.Set("User-Agent", defaultUA)
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Connection", "close")

	start := time.Now()
	resp, err := c.client.Do(req)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		r := failResult(0, latency, classifyErr(err, int(timeout.Seconds())))
		r.CheckedAt = time.Now()
		return r, nil
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	latency = time.Since(start).Milliseconds()
	if err != nil {
		r := failResult(resp.StatusCode, latency, "读取响应失败: "+classifyErr(err, int(timeout.Seconds())))
		r.CheckedAt = time.Now()
		return r, nil
	}
	if len(body) > maxBody {
		r := failResult(resp.StatusCode, latency, fmt.Sprintf("响应体超过 %dMB 限制", maxBody>>20))
		r.CheckedAt = time.Now()
		return r, nil
	}

	r := &Result{HTTPCode: resp.StatusCode, LatencyMs: latency, Size: len(body), CheckedAt: time.Now()}
	if resp.Request != nil && resp.Request.URL != nil {
		r.FinalURL = resp.Request.URL.String()
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		r.Status = model.StatusFail
		r.Msg = "HTTP " + strconv.Itoa(resp.StatusCode)
		return r, body
	}

	typ, detail, perr := parser.Parse(body)
	r.Type = typ
	r.Detail = detail
	if perr != nil {
		r.Status = model.StatusFail
		r.Msg = perr.Error()
		return r, body
	}
	r.Status = model.StatusOK
	r.Msg = parser.Summarize(detail, typ)
	return r, body
}

// FetchRaw 仅做 HTTP GET + SSRF 校验 + 体积上限，不解析内容（用于下载 git 归档等二进制）。
// maxBytes<=0 时用默认 20MB；返回 body 与结果（StatusOK 表示成功）。
func (c *Checker) FetchRaw(ctx context.Context, rawURL string, guardPrivate bool, maxBytes int64) (*Result, []byte) {
	u, err := parser.Normalize(rawURL)
	if err != nil {
		return failResult(0, 0, "地址格式非法: "+err.Error()), nil
	}
	if guardPrivate {
		if err := RejectPrivateHost(hostOf(u)); err != nil {
			return failResult(0, 0, "地址被拒绝: "+err.Error()), nil
		}
	}
	if maxBytes <= 0 {
		maxBytes = maxBody
	}
	timeout := c.timeout
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return failResult(0, 0, "构造请求失败: "+err.Error()), nil
	}
	req.Header.Set("User-Agent", defaultUA)
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Connection", "close")

	start := time.Now()
	resp, err := c.client.Do(req)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		r := failResult(0, latency, classifyErr(err, int(timeout.Seconds())))
		r.CheckedAt = time.Now()
		return r, nil
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	latency = time.Since(start).Milliseconds()
	if err != nil {
		r := failResult(resp.StatusCode, latency, "读取响应失败: "+err.Error())
		r.CheckedAt = time.Now()
		return r, nil
	}
	if len(body) > int(maxBytes) {
		r := failResult(resp.StatusCode, latency, fmt.Sprintf("响应体超过 %dMB 限制", maxBytes>>20))
		r.CheckedAt = time.Now()
		return r, nil
	}

	r := &Result{HTTPCode: resp.StatusCode, LatencyMs: latency, Size: len(body), CheckedAt: time.Now()}
	if resp.Request != nil && resp.Request.URL != nil {
		r.FinalURL = resp.Request.URL.String()
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		r.Status = model.StatusFail
		r.Msg = "HTTP " + strconv.Itoa(resp.StatusCode)
		return r, body
	}
	r.Status = model.StatusOK
	r.Msg = "OK"
	return r, body
}

func failResult(code int, latency int64, msg string) *Result {
	return &Result{Status: model.StatusFail, HTTPCode: code, LatencyMs: latency, Msg: msg}
}

func hostOf(u string) string {
	if p, err := url.Parse(u); err == nil {
		return p.Hostname()
	}
	return u
}

// classifyErr 失败原因分类文案
func classifyErr(err error, timeoutSec int) string {
	if err == nil {
		return ""
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "域名无法解析"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return fmt.Sprintf("请求超时（>%ds）", timeoutSec)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Sprintf("请求超时（>%ds）", timeoutSec)
	}
	var certErr *tls.CertificateVerificationError
	if errors.As(err, &certErr) {
		return "证书校验失败"
	}
	s := err.Error()
	switch {
	case strings.Contains(s, "certificate"), strings.Contains(s, "x509"):
		return "证书校验失败"
	case strings.Contains(s, "connection refused"):
		return "连接被拒绝"
	case strings.Contains(s, "too many redirects"), strings.Contains(s, "stopped after"):
		return "重定向过多"
	case strings.Contains(s, "EOF"), strings.Contains(s, "reset by peer"):
		return "连接被中断"
	case strings.Contains(s, "no such host"):
		return "域名无法解析"
	default:
		if len(s) > 120 {
			s = s[:120]
		}
		return s
	}
}

// RejectPrivateHost SSRF 防护：拒绝环回/内网/链路本地地址。
// IP 字面量直接判定；域名解析后判定全部结果地址。
func RejectPrivateHost(host string) error {
	if strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".localhost") {
		return fmt.Errorf("内网地址（localhost）")
	}
	if ip := net.ParseIP(host); ip != nil {
		if isPrivateIP(ip) {
			return fmt.Errorf("内网地址（%s）", host)
		}
		return nil
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		// 解析失败交给后续请求报真实错误
		return nil
	}
	for _, ip := range ips {
		if isPrivateIP(ip) {
			return fmt.Errorf("域名解析到内网地址（%s）", ip.String())
		}
	}
	return nil
}

func isPrivateIP(ip net.IP) bool {
	privateRanges := []string{
		"127.0.0.0/8", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16",
		"169.254.0.0/16", "0.0.0.0/8", "100.64.0.0/10", "::1/128", "fc00::/7", "fe80::/10",
	}
	for _, cidr := range privateRanges {
		if _, network, err := net.ParseCIDR(cidr); err == nil && network.Contains(ip) {
			return true
		}
	}
	return false
}
