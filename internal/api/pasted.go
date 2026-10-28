package api

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"tvbox-manager/internal/model"
	"tvbox-manager/internal/parser"
)

/*
「粘贴的单仓配置」本地托管：

粘贴进来的单仓内容本身没有 URL，而系统里的源必须是一个地址（探活、
站点源展开、订阅输出都建立在 URL 上）。托管是「直接使用」的落地方式：
内容存到 PastedDir 下的 <id>.json，由 GET /configs/<id>.json 对外提供
（与订阅接口同为无鉴权），再自动创建一条指向该地址的源——整条既有
管线（探活 → 生成站点源 → 订阅下发）原样复用，删除源时同步清理文件。
*/

var pastedNameRe = regexp.MustCompile(`^[a-f0-9]{16}\.json$`)

// pastedURLPrefix 托管文件在源 URL 里的路径特征（删除源时识别用）
const pastedURLPrefix = "/configs/"

// savePastedConfig 把文本写入托管目录，返回文件名（<16位hex>.json）。
// 写入前应已完成解析校验；这里只负责原子落盘。
func (a *API) savePastedConfig(text string) (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	name := hex.EncodeToString(b) + ".json"
	if err := os.MkdirAll(a.PastedDir, 0o755); err != nil {
		return "", err
	}
	tmp := filepath.Join(a.PastedDir, name+".tmp")
	if err := os.WriteFile(tmp, []byte(text), 0o644); err != nil {
		return "", err
	}
	return name, os.Rename(tmp, filepath.Join(a.PastedDir, name))
}

// cleanupPasted 源删除后清理其托管文件（URL 含 /configs/ 前缀才算托管源）
func (a *API) cleanupPasted(srcURL string) {
	if a.PastedDir == "" || !strings.Contains(srcURL, pastedURLPrefix) {
		return
	}
	name := srcURL[strings.LastIndex(srcURL, pastedURLPrefix)+len(pastedURLPrefix):]
	if !pastedNameRe.MatchString(name) {
		return
	}
	_ = os.Remove(filepath.Join(a.PastedDir, name)) // 清理失败不打断删除流程
}

// isLocalURL 地址是否属于本地托管源（本地源不参与检测，始终可用）
func (a *API) isLocalURL(rawURL string) bool {
	norm, err := parser.Normalize(rawURL)
	if err != nil {
		return false
	}
	if src, found := a.st.GetByURL(norm); found {
		return src.Type == model.TypeLocal
	}
	return false
}

// handlePastedConfig GET /configs/{name}：对外提供托管配置（无鉴权，与订阅一致）。
// 名称白名单校验，杜绝路径穿越。
func (a *API) handlePastedConfig(w http.ResponseWriter, r *http.Request) {
	if a.PastedDir == "" {
		notFound(w, "本地托管未启用")
		return
	}
	name := r.PathValue("name")
	if !pastedNameRe.MatchString(name) {
		notFound(w, "配置不存在")
		return
	}
	body, err := os.ReadFile(filepath.Join(a.PastedDir, name))
	if err != nil {
		notFound(w, "配置不存在")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(body)
}

// baseURL 拼托管源的访问地址用：scheme + host（反代场景尊重 X-Forwarded-Proto）
func baseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	host := r.Host
	if host == "" {
		host = "127.0.0.1"
	}
	return scheme + "://" + host
}
