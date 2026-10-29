package api

import (
	"strings"
	"testing"

	"tvbox-manager/internal/model"
)

// pasteLocal 通过 JSON 粘贴导入建一条本地源（单仓），返回源 id 与托管地址
func (e *testEnv) pasteLocal(name, text string) (string, string) {
	e.t.Helper()
	code, body := e.doJSON("POST", "/api/sources/import/json", map[string]interface{}{
		"text": text, "name": name,
	})
	if code != 200 {
		e.t.Fatalf("导入本地源失败 code=%d body=%v", code, body)
	}
	d := dataOf(e.t, body)
	id, _ := d["id"].(string)
	u, _ := d["selfHosted"].(string)
	if id == "" || u == "" {
		e.t.Fatalf("导入返回缺 id/selfHosted: %v", body)
	}
	return id, u
}

const singleRepoJSON = `{"spider":"http://x/s.jar","sites":[` +
	`{"key":"a","name":"A站","type":1,"api":"http://x/api.php"},` +
	`{"key":"b","name":"B站","type":1,"api":"http://y/api.php"}],` +
	`"lives":[{"name":"直播1"}],"parses":[{"name":"p1"}]}`

// 只有本地源能读配置原文；普通地址源一律拒绝（前端据此决定是否显示编辑区）
func TestSourceConfigOnlyForLocal(t *testing.T) {
	e := newEnv(t)
	id, _ := e.pasteLocal("粘贴仓", singleRepoJSON)

	code, body := e.doJSON("GET", "/api/sources/"+id+"/config", nil)
	if code != 200 {
		t.Fatalf("本地源读配置应 200，得到 %d: %v", code, body)
	}
	d := dataOf(t, body)
	content, _ := d["content"].(string)
	if !strings.Contains(content, `"sites"`) {
		t.Fatalf("配置原文异常: %v", content)
	}
	if det, _ := d["detail"].(map[string]interface{}); det == nil || num(det, "sites") != 2 {
		t.Fatalf("应回带解析摘要（2 站点）: %v", d["detail"])
	}

	// 普通地址源（非本地托管）
	plainID := e.addSrc("普通源", "http://plain.com/tv.json", model.TypeSingle)
	code, body = e.doJSON("GET", "/api/sources/"+plainID+"/config", nil)
	if code != 400 {
		t.Fatalf("普通源读配置应 400，得到 %d: %v", code, body)
	}
}

// 编辑内容：校验通过 → 覆盖同一个托管文件（地址不变）→ 摘要刷新 → 订阅内容随之变化
func TestSourceEditContent(t *testing.T) {
	e := newEnv(t)
	id, selfURL := e.pasteLocal("粘贴仓", singleRepoJSON)
	name := selfURL[strings.LastIndex(selfURL, "/")+1:]

	newText := `{"sites":[` +
		`{"key":"a","name":"A站","type":1,"api":"http://x/api.php"},` +
		`{"key":"c","name":"C站","type":1,"api":"http://z/api.php"},` +
		`{"key":"d","name":"D站","type":0,"api":"csp_AppYsV2"}],` +
		`"lives":[]}`

	code, body := e.doJSON("PUT", "/api/sources/"+id, map[string]interface{}{"content": newText})
	if code != 200 {
		t.Fatalf("保存配置应 200，得到 %d: %v", code, body)
	}
	d := dataOf(t, body)
	if det, _ := d["detail"].(map[string]interface{}); det == nil || num(det, "sites") != 3 {
		t.Fatalf("摘要应更新为 3 站点: %v", d["detail"])
	}
	if u, _ := d["url"].(string); u != selfURL {
		t.Fatalf("托管地址不应改变: %q != %q", u, selfURL)
	}
	if st, _ := d["status"].(string); st != model.StatusOK {
		t.Fatalf("本地源保存后应保持可用，得到 %q", st)
	}

	// 托管文件已更新，且仍是合法 JSON
	code, raw := e.do("GET", "/configs/"+name, "", "", "")
	if code != 200 {
		t.Fatalf("托管文件拉取失败 code=%d", code)
	}
	if !strings.Contains(raw, `"C站"`) || strings.Contains(raw, `"B站"`) {
		t.Fatalf("托管内容未更新: %s", raw)
	}
	if !strings.Contains(raw, `"api":"csp_AppYsV2"`) {
		t.Fatalf("内容应原样保存（不改写站点字段）: %s", raw)
	}

	// 站点源还是自己那一条，地址不变 → 客户端订阅地址不用动
	flats := e.flatsOf(id)
	if len(flats) != 1 || flats[0].URL != selfURL {
		t.Fatalf("站点源应保持 1 条且地址不变: %+v", flats)
	}
}

// 内容不合法（解析不了）→ 拒绝保存，托管文件保持原样
func TestSourceEditContentInvalidRejected(t *testing.T) {
	e := newEnv(t)
	id, selfURL := e.pasteLocal("粘贴仓", singleRepoJSON)
	name := selfURL[strings.LastIndex(selfURL, "/")+1:]

	for _, bad := range []string{
		"", // 空内容
		"这不是 JSON，只是一段说明文字",     // 完全不是 JSON
		`{"sites":[{"name":"A"`, // 截断
	} {
		code, body := e.doJSON("PUT", "/api/sources/"+id, map[string]interface{}{"content": bad})
		if code != 400 {
			t.Fatalf("非法内容应 400，得到 %d: %v（内容 %q）", code, body, bad)
		}
	}

	_, raw := e.do("GET", "/configs/"+name, "", "", "")
	if !strings.Contains(raw, `"B站"`) {
		t.Fatalf("被拒绝的保存不应改动托管文件: %s", raw)
	}
}

// 多仓内容不能塞进本地源（本地源只托管单仓）
func TestSourceEditContentMultiRejected(t *testing.T) {
	e := newEnv(t)
	id, _ := e.pasteLocal("粘贴仓", singleRepoJSON)
	multi := `{"urls":[{"url":"http://a.com/1.json","name":"A"},{"url":"http://b.com/2.json","name":"B"}]}`
	code, body := e.doJSON("PUT", "/api/sources/"+id, map[string]interface{}{"content": multi})
	if code != 400 {
		t.Fatalf("多仓内容应 400，得到 %d: %v", code, body)
	}
	if msg, _ := body["msg"].(string); !strings.Contains(msg, "多仓") {
		t.Fatalf("错误提示应说明是多仓配置: %v", body)
	}
}

// 普通地址源不接受 content 字段
func TestSourceEditContentNonLocalRejected(t *testing.T) {
	e := newEnv(t)
	id := e.addSrc("普通源", "http://plain.com/tv.json", model.TypeSingle)
	code, body := e.doJSON("PUT", "/api/sources/"+id, map[string]interface{}{"content": singleRepoJSON})
	if code != 400 {
		t.Fatalf("普通源带 content 应 400，得到 %d: %v", code, body)
	}
}

// 换地址与换内容不能同时提交（避免内容写进即将被删的托管文件）
func TestSourceEditContentAndURLTogetherRejected(t *testing.T) {
	e := newEnv(t)
	id, _ := e.pasteLocal("粘贴仓", singleRepoJSON)
	code, body := e.doJSON("PUT", "/api/sources/"+id, map[string]interface{}{
		"content": singleRepoJSON, "url": "http://other.com/tv.json",
	})
	if code != 400 {
		t.Fatalf("同时改地址与内容应 400，得到 %d: %v", code, body)
	}
}

// 本地源换地址 → 脱离本地托管：类型回退未定型、托管文件被清理、站点源随之消失
func TestLocalSourceChangeURLDetaches(t *testing.T) {
	e := newEnv(t)
	id, selfURL := e.pasteLocal("粘贴仓", singleRepoJSON)
	name := selfURL[strings.LastIndex(selfURL, "/")+1:]

	code, body := e.doJSON("PUT", "/api/sources/"+id, map[string]interface{}{"url": "http://new.com/tv.json"})
	if code != 200 {
		t.Fatalf("改地址应成功，得到 %d: %v", code, body)
	}
	src, _ := e.st.Get(id)
	if src.Type == model.TypeLocal {
		t.Fatalf("换地址后不应仍是本地源: %+v", src)
	}
	if src.Type != model.TypeUnknown {
		t.Fatalf("换地址后应回退未定型（等探活），得到 %q", src.Type)
	}
	if code, _ := e.do("GET", "/configs/"+name, "", "", ""); code != 404 {
		t.Fatalf("脱离托管后旧文件应被清理（404），得到 %d", code)
	}
	if n := e.flatCount(id); n != 0 {
		t.Fatalf("未定型源不应生成站点源，得到 %d", n)
	}
}

// 编辑内容时保留其它字段：名称/分组/启停都不动
func TestSourceEditContentKeepsMetadata(t *testing.T) {
	e := newEnv(t)
	id, _ := e.pasteLocal("粘贴仓", singleRepoJSON)
	before, _ := e.st.Get(id)

	code, body := e.doJSON("PUT", "/api/sources/"+id, map[string]interface{}{
		"content": `{"sites":[{"key":"a","name":"只有一个","type":1,"api":"http://x/api.php"}]}`,
	})
	if code != 200 {
		t.Fatalf("保存失败: %v", body)
	}
	after, _ := e.st.Get(id)
	if after.Name != before.Name || after.Group != before.Group || after.Enabled != before.Enabled || after.URL != before.URL {
		t.Fatalf("编辑内容不应改动其它字段: before=%+v after=%+v", before, after)
	}
	if after.Detail == nil || after.Detail.Sites != 1 {
		t.Fatalf("摘要应更新为 1 站点: %+v", after.Detail)
	}
}
