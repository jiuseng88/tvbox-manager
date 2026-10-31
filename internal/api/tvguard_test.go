package api

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"testing/fstest"

	"tvbox-manager/internal/scheduler"
	"tvbox-manager/internal/store"
)

// req 发一个请求，返回状态码与响应头（鉴权要断言 Location，e.do 不暴露头）
func (e *testEnv) req(method, path, token, cookie string, html bool) *httptest.ResponseRecorder {
	e.t.Helper()
	r := httptest.NewRequest(method, path, nil)
	if token != "" {
		r.Header.Set("X-Token", token)
	}
	if cookie != "" {
		r.AddCookie(&http.Cookie{Name: CookieName, Value: url.QueryEscape(cookie)})
	}
	if html {
		r.Header.Set("Accept", "text/html,application/xhtml+xml")
		r.Header.Set("Sec-Fetch-Mode", "navigate")
	}
	rec := httptest.NewRecorder()
	e.api.ServeHTTP(rec, r)
	return rec
}

// TestTVRequiresLogin /tv、静态资源、/tvapi/* 三处都要校验登录态
func TestTVRequiresLogin(t *testing.T) {
	e := newEnv(t)
	tok := e.token

	// 1. 页面请求未登录：302 到后台登录页，并带回原地址
	for _, p := range []string{"/tv", "/tv/", "/tv/index.html", "/tv/live.html"} {
		rec := e.req("GET", p, "", "", true)
		if rec.Code != http.StatusFound {
			t.Fatalf("%s 未登录应 302，实际 %d", p, rec.Code)
		}
		loc := rec.Header().Get("Location")
		if !strings.Contains(loc, "login=1") || !strings.Contains(loc, "next=") {
			t.Fatalf("%s 跳转地址不对: %s", p, loc)
		}
	}

	// 2. 接口/资源请求未登录：401 JSON，不能吐 HTML
	for _, p := range []string{"/tv/js/app.js", "/tvapi/sources", "/tvapi/live", "/tvapi/media?url=http://a/b.m3u8"} {
		rec := e.req("GET", p, "", "", false)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s 未登录应 401，实际 %d（%s）", p, rec.Code, rec.Body.String())
		}
	}

	// 3. Cookie 登录态：页面与接口都放行
	if rec := e.req("GET", "/tv/", "", tok, true); rec.Code != http.StatusOK {
		t.Fatalf("带 Cookie 访问 /tv/ 应 200，实际 %d", rec.Code)
	}
	if rec := e.req("GET", "/tvapi/live", "", tok, false); rec.Code != http.StatusOK {
		t.Fatalf("带 Cookie 访问 /tvapi/live 应 200，实际 %d", rec.Code)
	}

	// 4. 错误 Cookie 不放行
	if rec := e.req("GET", "/tv/", "", "wrong-token", true); rec.Code != http.StatusFound {
		t.Fatalf("错误 Cookie 应 302，实际 %d", rec.Code)
	}

	// 5. 后台接口不受影响：无令牌仍 401，带令牌正常
	if rec := e.req("GET", "/api/sources", "", "", false); rec.Code != http.StatusUnauthorized {
		t.Fatalf("/api/sources 无令牌应 401，实际 %d", rec.Code)
	}
	if rec := e.req("GET", "/api/sources", tok, "", false); rec.Code != http.StatusOK {
		t.Fatalf("/api/sources 带令牌应 200，实际 %d", rec.Code)
	}
}

// TestTVDisabledHidesPath showTv=false 时菜单关掉、路径也真关掉（已登录同样 404）
func TestTVDisabledHidesPath(t *testing.T) {
	e := newEnv(t)
	tok := e.token

	cfg := e.st.Config()
	cfg.ShowTV = false
	if err := e.st.UpdateConfig(cfg); err != nil {
		t.Fatal(err)
	}

	paths := []string{"/tv", "/tv/", "/tv/index.html", "/tv/js/app.js",
		"/tvapi/sources", "/tvapi/live", "/tvapi/media?url=http://a/b.m3u8"}
	for _, p := range paths {
		// 页面与接口都试，且都带合法令牌 —— 关闭后连登录用户也不能访问
		for _, html := range []bool{true, false} {
			rec := e.req("GET", p, tok, tok, html)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("showTv=false 时 %s 应 404，实际 %d", p, rec.Code)
			}
		}
	}

	// 后台与订阅不受开关影响
	if rec := e.req("GET", "/api/sources", tok, "", false); rec.Code != http.StatusOK {
		t.Fatalf("关闭看片页后 /api/sources 仍应 200，实际 %d", rec.Code)
	}
	if rec := e.req("GET", "/sub", "", "", false); rec.Code != http.StatusOK {
		t.Fatalf("关闭看片页后订阅仍应 200，实际 %d", rec.Code)
	}

	// /healthz 要如实反映开关，前端靠它决定菜单是否渲染
	rec := e.req("GET", "/healthz", "", "", false)
	if !strings.Contains(rec.Body.String(), `"tvEnabled":false`) {
		t.Fatalf("/healthz 应报告 tvEnabled=false，实际 %s", rec.Body.String())
	}
}

// TestTVNoAuthMode 未设令牌（无鉴权模式）时没有"登录"概念，看片页直接可用。
// 不能用 UpdateConfig 清空令牌——它对空值有保护（防止设置页误清空），所以这里直接新建一个空令牌实例。
func TestTVNoAuthMode(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := st.Config()
	if cfg.Token != "" {
		t.Fatalf("新实例不应有令牌，实际 %q", cfg.Token)
	}
	sch := scheduler.Start(st, &cfg)
	webFS := fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("<html>ok</html>")}}
	e := &testEnv{t: t, st: st, api: New(st, sch, "test", webFS).Router()}

	for _, p := range []string{"/tv/", "/tvapi/live"} {
		if rec := e.req("GET", p, "", "", false); rec.Code != http.StatusOK {
			t.Fatalf("无鉴权模式下 %s 应 200，实际 %d", p, rec.Code)
		}
	}
}

// TestConfigPutKeepsShowTVWhenOmitted 旧调用方 PUT 配置时没带 showTv 字段，
// 不能因为 bool 零值把看片页静默关掉
func TestConfigPutKeepsShowTVWhenOmitted(t *testing.T) {
	e := newEnv(t)
	cfg := e.st.Config()
	cfg.ShowTV = true
	if err := e.st.UpdateConfig(cfg); err != nil {
		t.Fatal(err)
	}

	// 不带 showTv 的旧格式请求体
	body := `{"port":8080,"subPath":"/sub","checkIntervalMinutes":360,` +
		`"checkTimeoutSeconds":10,"checkConcurrency":10,"startupCheck":false,` +
		`"autoDisableAfterFails":5,"insecureSkipVerify":true,"checkSubUrls":false}`
	if code, msg := e.do("PUT", "/api/config", e.token, body, "application/json"); code != http.StatusOK {
		t.Fatalf("PUT 配置失败 %d %s", code, msg)
	}
	if !e.st.Config().ShowTV {
		t.Fatal("缺省 showTv 的 PUT 不应关闭看片页")
	}

	// 显式传 false 才关闭
	body = `{"port":8080,"subPath":"/sub","checkIntervalMinutes":360,` +
		`"checkTimeoutSeconds":10,"checkConcurrency":10,"showTv":false}`
	if code, msg := e.do("PUT", "/api/config", e.token, body, "application/json"); code != http.StatusOK {
		t.Fatalf("PUT 配置失败 %d %s", code, msg)
	}
	if e.st.Config().ShowTV {
		t.Fatal("显式 showTv=false 应生效")
	}
}
