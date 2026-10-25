package checker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"tvbox-manager/internal/model"
)

func newChecker() *Checker {
	cfg := model.DefaultConfig()
	cfg.CheckTimeoutSeconds = 3
	return New(&cfg)
}

const singleBody = `{"spider":"http://x/s.jar","sites":[{"key":"a","name":"A站","type":1,"api":"http://x/api.php"}]}`

func TestFetchOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") == "" {
			t.Error("请求应带 User-Agent")
		}
		_, _ = w.Write([]byte(singleBody))
	}))
	defer srv.Close()

	res, body := newChecker().Fetch(context.Background(), srv.URL, false)
	if res.Status != model.StatusOK {
		t.Fatalf("期望 ok: status=%s msg=%s", res.Status, res.Msg)
	}
	if res.Type != model.TypeSingle || res.Detail.Sites != 1 {
		t.Fatalf("解析错误: %s %+v", res.Type, res.Detail)
	}
	if res.LatencyMs < 0 || len(body) == 0 {
		t.Fatal("延迟与响应体应记录")
	}
}

func TestFetchHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	res, _ := newChecker().Fetch(context.Background(), srv.URL, false)
	if res.Status != model.StatusFail || res.Msg != "HTTP 403" {
		t.Fatalf("期望 HTTP 403: %+v", res)
	}
}

func TestFetchNotJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<html>禁止访问</html>"))
	}))
	defer srv.Close()
	res, _ := newChecker().Fetch(context.Background(), srv.URL, false)
	if res.Status != model.StatusFail || !strings.Contains(res.Msg, "非 JSON") {
		t.Fatalf("期望非 JSON 错误: %+v", res)
	}
}

func TestFetchStructureIncomplete(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"wallpaper":"http://x/bg.jpg"}`))
	}))
	defer srv.Close()
	res, _ := newChecker().Fetch(context.Background(), srv.URL, false)
	if res.Status != model.StatusFail || !strings.Contains(res.Msg, "结构不完整") {
		t.Fatalf("期望结构不完整: %+v", res)
	}
}

func TestFetchTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // 挂起直到客户端超时断开（handler 必须可返回，否则 Close 阻塞）
	}))
	defer srv.Close()
	res, _ := newChecker().Fetch(context.Background(), srv.URL, false)
	if res.Status != model.StatusFail || !strings.Contains(res.Msg, "超时") {
		t.Fatalf("期望超时: %+v", res)
	}
}

func TestFetchDNSFail(t *testing.T) {
	res, _ := newChecker().Fetch(context.Background(), "http://this-domain-definitely-not-exist-xyz.example", false)
	if res.Status != model.StatusFail || !strings.Contains(res.Msg, "域名无法解析") {
		t.Fatalf("期望域名无法解析: %+v", res)
	}
}

func TestFetchRedirectFinalURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/a":
			http.Redirect(w, r, "/b", http.StatusFound)
		default:
			_, _ = w.Write([]byte(singleBody))
		}
	}))
	defer srv.Close()
	res, _ := newChecker().Fetch(context.Background(), srv.URL+"/a", false)
	if res.Status != model.StatusOK || !strings.HasSuffix(res.FinalURL, "/b") {
		t.Fatalf("重定向后应记录 finalURL: %+v", res)
	}
}

func TestSSRFGuard(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "10.1.2.3", "172.16.0.9", "192.168.1.1",
		"169.254.169.254", "0.0.0.0", "::1", "localhost",
	}
	for _, h := range blocked {
		if err := RejectPrivateHost(h); err == nil {
			t.Errorf("内网地址 %s 应被拒绝", h)
		}
	}
	if err := RejectPrivateHost("8.8.8.8"); err != nil {
		t.Errorf("公网 IP 不应被拒绝: %v", err)
	}
}

func TestSSRFOnPreview(t *testing.T) {
	// guard=true 时 httptest（127.0.0.1）会被拦截
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(singleBody))
	}))
	defer srv.Close()
	res, _ := newChecker().Fetch(context.Background(), srv.URL, true)
	if res.Status != model.StatusFail || !strings.Contains(res.Msg, "拒绝") {
		t.Fatalf("guard 应拦截内网地址: %+v", res)
	}
}
