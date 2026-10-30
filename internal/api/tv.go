package api

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"tvbox-manager/internal/checker"
	"tvbox-manager/internal/model"
	"tvbox-manager/internal/parser"
)

// /tv 看片页的后端支撑：
//   - GET /tvapi/sources 聚合平铺层里所有可用源的苹果CMS站点接口，供前端填充搜索源；
//   - GET /tvapi/proxy   CMS 接口的服务端代理（浏览器直连会被 CORS 拦住）。
//
// 两个端点均无鉴权，与订阅出口同级；代理出口复用 checker 的 SSRF 防护。

// tvSourceItem /tv 下发的源条目
type tvSourceItem struct {
	Name   string `json:"name"`
	API    string `json:"api"`
	Detail string `json:"detail,omitempty"`
}

const (
	tvAggTTL     = 5 * time.Minute  // 聚合缓存：管理端改动最迟 5 分钟反映到 /tv
	tvMaxSources = 100              // 下发上限，与前端的并发搜索承受力对齐
	tvFetchLimit = 6                // 仓内数据缺失时现场补拉的仓数上限
	tvFetchDoor  = 6 * time.Second  // 单仓补拉超时
)

var (
	tvAggMu    sync.Mutex
	tvAggAt    time.Time
	tvAggItems []tvSourceItem
)

// handleTVSources 聚合源列表。优先取存储层（探活时已解析入库的 CmsSites），
// 为空时（旧数据未重检/刚部署）现场补拉少量单仓，结果缓存 tvAggTTL。
func (a *API) handleTVSources(w http.ResponseWriter, r *http.Request) {
	items := a.tvAggregate(r.Context())
	ok(w, map[string]interface{}{"sources": items, "count": len(items)})
}

func (a *API) tvAggregate(ctx context.Context) []tvSourceItem {
	tvAggMu.Lock()
	defer tvAggMu.Unlock()
	if tvAggItems != nil && time.Since(tvAggAt) < tvAggTTL {
		return tvAggItems
	}
	items := a.tvFromStore()
	if len(items) == 0 {
		items = a.tvFetchMissing(ctx)
	}
	tvAggAt = time.Now()
	tvAggItems = items
	return items
}

// tvFromStore 从存储层聚合：所有启用且探活成功的源的 CmsSites，按 API 地址去重。
func (a *API) tvFromStore() []tvSourceItem {
	seen := make(map[string]bool, 64)
	out := make([]tvSourceItem, 0, 64)
	for _, s := range a.st.AllSources() {
		if !s.Enabled || s.Detail == nil || s.Status != model.StatusOK {
			continue
		}
		for _, e := range s.Detail.CmsSites {
			if e.API == "" || seen[e.API] {
				continue
			}
			if len(out) >= tvMaxSources {
				return out
			}
			seen[e.API] = true
			it := tvSourceItem{Name: e.Name, API: e.API}
			if e.Adult {
				it.Name += "（成人）"
			}
			out = append(out, it)
		}
	}
	return out
}

// tvFetchMissing 存储层一条 CMS 站点都没有时的兜底：挑启用中的单仓现场拉取解析。
// 只做读取，不回写存储（探活任务下次运行时自然会补齐 Detail.CmsSites）。
func (a *API) tvFetchMissing(ctx context.Context) []tvSourceItem {
	candidates := make([]model.Source, 0, tvFetchLimit)
	for _, s := range a.st.AllSources() {
		if !s.Enabled || s.Type != model.TypeSingle {
			continue
		}
		candidates = append(candidates, s)
		if len(candidates) >= tvFetchLimit {
			break
		}
	}
	if len(candidates) == 0 {
		return nil
	}

	seen := make(map[string]bool, 64)
	out := make([]tvSourceItem, 0, 64)
	ck := a.sch.Checker()
	for _, s := range candidates {
		if len(out) >= tvMaxSources {
			break
		}
		fctx, cancel := context.WithTimeout(ctx, tvFetchDoor)
		_, body := ck.FetchT(fctx, s.URL, false, int(tvFetchDoor.Seconds()))
		cancel()
		if len(body) == 0 {
			continue
		}
		typ, detail, err := parser.Parse(body)
		if err != nil || typ != model.TypeSingle || detail == nil {
			continue
		}
		for _, e := range detail.CmsSites {
			if e.API == "" || seen[e.API] {
				continue
			}
			if len(out) >= tvMaxSources {
				return out
			}
			seen[e.API] = true
			it := tvSourceItem{Name: e.Name, API: e.API}
			if e.Adult {
				it.Name += "（成人）"
			}
			out = append(out, it)
		}
	}
	return out
}

// ---- /tvapi/proxy ----

var tvProxyClient = &http.Client{Timeout: 12 * time.Second}

const tvProxyMaxBody = 10 << 20

// handleTVProxy 通用 GET 代理：前端搜索/详情统一走这里，规避 CMS 接口的 CORS 限制。
// 安全边界：仅公网 http/https，复用 checker.RejectPrivateHost 拦内网；响应体限 10MB。
func (a *API) handleTVProxy(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("url")
	if raw == "" {
		badRequest(w, "缺少 url 参数")
		return
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		badRequest(w, "仅支持公网 http/https 地址")
		return
	}
	if err := checker.RejectPrivateHost(u.Hostname()); err != nil {
		badRequest(w, "地址被拒绝: "+err.Error())
		return
	}

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, raw, nil)
	if err != nil {
		badRequest(w, "构造请求失败: "+err.Error())
		return
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/122.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "application/json, text/html, */*")

	resp, err := tvProxyClient.Do(req)
	if err != nil {
		writeErr(w, http.StatusBadGateway, 502, "上游请求失败: "+classifyProxyErr(err))
		return
	}
	defer resp.Body.Close()

	// 先缓冲再输出：上游中途断流/超时能正确回 502，而不是吐出半截 200
	body, err := io.ReadAll(io.LimitReader(resp.Body, tvProxyMaxBody+1))
	if err != nil && len(body) == 0 {
		writeErr(w, http.StatusBadGateway, 502, "读取上游响应失败: "+classifyProxyErr(err))
		return
	}

	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/json; charset=utf-8"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
}

func classifyProxyErr(err error) string {
	if uerr, ok := err.(*url.Error); ok {
		if uerr.Timeout() {
			return "上游超时"
		}
		return uerr.Err.Error()
	}
	return err.Error()
}

// ---- /tvapi/media：播放链路代理 ----
//
// 浏览器端 hls.js 拉 m3u8 会被 CORS/Referer 防盗链拦住，必须服务端代理。
// 清单（m3u8）整体缓冲并改写：所有分片/密钥/子清单地址改写回本代理，保证
// hls.js 后续的每一跳都不直连上游；分片（ts/mp4/key）走流式透传不落内存。

var tvMediaClient = &http.Client{
	Timeout:       60 * time.Second,
	CheckRedirect: tvCheckRedirect,
}

const (
	tvMediaMaxBody  = 64 << 20 // 单个分片上限：ts 一般 2-10MB，留足余量
	tvM3U8MaxBody   = 4 << 20  // 清单体上限（绝大多数 <100KB）
	tvStreamTimeout = 5 * time.Minute
)

// tvCheckRedirect 重定向逐跳过 SSRF 校验，防止公网 302 跳内网。
func tvCheckRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 5 {
		return errors.New("已停止跟踪重定向")
	}
	if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
		return errors.New("仅支持 http/https")
	}
	if err := checker.RejectPrivateHost(req.URL.Hostname()); err != nil {
		return err
	}
	return nil
}

// handleTVMedia 播放媒体代理：m3u8 改写后返回，其余流式透传。
func (a *API) handleTVMedia(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("url")
	if raw == "" {
		badRequest(w, "缺少 url 参数")
		return
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		badRequest(w, "仅支持公网 http/https 地址")
		return
	}
	if err := checker.RejectPrivateHost(u.Hostname()); err != nil {
		badRequest(w, "地址被拒绝: "+err.Error())
		return
	}

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, raw, nil)
	if err != nil {
		badRequest(w, "构造请求失败: "+err.Error())
		return
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/122.0.0.0 Safari/537.36")
	// 不带 Referer：多数 CDN 防盗链只拦陌生 Referer，无 Referer 反而放行

	resp, err := tvMediaClient.Do(req)
	if err != nil {
		writeErr(w, http.StatusBadGateway, 502, "上游请求失败: "+classifyProxyErr(err))
		return
	}
	defer resp.Body.Close()

	ct := resp.Header.Get("Content-Type")
	br := bufio.NewReaderSize(resp.Body, 8192)
	head, _ := br.Peek(8)
	isM3U8 := strings.Contains(strings.ToLower(ct), "mpegurl") ||
		strings.HasPrefix(strings.TrimLeft(string(head), " \t\ufeff"), "#EXTM3U")

	if isM3U8 {
		body, _ := io.ReadAll(io.LimitReader(br, tvM3U8MaxBody))
		base := resp.Request.URL // 重定向后的最终地址才是改写基准
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(rewriteM3U8(string(body), base)))
		return
	}

	// 分片/密钥：流式透传
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		w.Header().Set("Content-Length", cl)
	}
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(resp.StatusCode)

	ctx, cancel := context.WithTimeout(r.Context(), tvStreamTimeout)
	defer cancel()
	_, _ = io.Copy(w, io.LimitReader(newCtxReader(ctx, br), tvMediaMaxBody))
}

// newCtxReader 透传阶段也尊重整体超时，避免个别慢分片把连接挂死。
func newCtxReader(ctx context.Context, r io.Reader) io.Reader {
	return &ctxReader{ctx: ctx, r: r}
}

type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

var tvURIAttrRe = regexp.MustCompile(`URI="([^"]+)"`)

// rewriteM3U8 把清单里所有资源地址改写回 /tvapi/media 代理。
// 覆盖：分片行、EXT-X-STREAM-INF 的子清单行、EXT-X-KEY / EXT-X-MAP / EXT-X-MEDIA 的 URI 属性。
func rewriteM3U8(body string, base *url.URL) string {
	if base == nil {
		return body
	}
	lines := strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n")
	for i, ln := range lines {
		t := strings.TrimSpace(ln)
		if t == "" {
			continue
		}
		if strings.HasPrefix(t, "#") {
			if strings.Contains(t, `URI="`) {
				lines[i] = tvURIAttrRe.ReplaceAllStringFunc(t, func(m string) string {
					sub := tvURIAttrRe.FindStringSubmatch(m)
					if len(sub) != 2 {
						return m
					}
					return `URI="` + tvProxify(sub[1], base) + `"`
				})
			}
			continue
		}
		lines[i] = tvProxify(t, base)
	}
	return strings.Join(lines, "\n")
}

// tvProxify 相对地址 → 绝对地址 → 代理地址；无法处理时原样返回。
func tvProxify(raw string, base *url.URL) string {
	if raw == "" || strings.HasPrefix(raw, "data:") {
		return raw
	}
	ref, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	abs := base.ResolveReference(ref)
	if abs.Scheme != "http" && abs.Scheme != "https" {
		return raw
	}
	if err := checker.RejectPrivateHost(abs.Hostname()); err != nil {
		return raw
	}
	return "/tvapi/media?url=" + url.QueryEscape(abs.String())
}
