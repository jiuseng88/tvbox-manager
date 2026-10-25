package store

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"tvbox-manager/internal/model"
)

// Store JSON 文件持久化：内存缓存 + RWMutex + 原子写（临时文件 + Rename）。
type Store struct {
	mu       sync.RWMutex
	dataFile string
	cfgFile  string
	data     *model.Data
	cfg      *model.Config
}

// Filter 列表过滤条件，全部字段可选
type Filter struct {
	Keyword string // 名称/URL/备注 模糊匹配
	Type    string // single / multi / unknown
	Status  string // ok / fail / unknown
	Group   string
	Enabled string // "true" / "false"
	Page    int
	Size    int
	Sort    string // 默认 sort+name；"failFirst" 失效置顶
}

// CheckResult 单源探活结果回写
type CheckResult struct {
	Status    string
	HTTPCode  int
	LatencyMs int64
	Msg       string
	Type      string
	Detail    *model.Detail
}

// New 打开/初始化数据目录
func New(dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("创建数据目录失败: %w", err)
	}
	s := &Store{
		dataFile: filepath.Join(dataDir, "data.json"),
		cfgFile:  filepath.Join(dataDir, "config.json"),
		data:     &model.Data{Version: 1},
		cfg:      nil,
	}
	if err := s.loadConfig(); err != nil {
		return nil, err
	}
	if err := s.loadData(); err != nil {
		return nil, err
	}
	return s, nil
}

// ---------- 配置 ----------

func (s *Store) loadConfig() error {
	cfg := model.DefaultConfig()
	if b, err := os.ReadFile(s.cfgFile); err == nil {
		if err := json.Unmarshal(b, &cfg); err != nil {
			// 配置损坏不阻塞启动：备份后用默认值
			bak := s.cfgFile + ".corrupt." + time.Now().Format("20060102150405")
			_ = os.Rename(s.cfgFile, bak)
			fmt.Printf("[store] config.json 解析失败，已备份到 %s，使用默认配置: %v\n", bak, err)
			cfg = model.DefaultConfig()
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	applyEnv(&cfg)
	firstRun := false
	if cfg.Token == "" {
		cfg.Token = randToken(16)
		firstRun = true
	}
	s.cfg = &cfg
	if firstRun {
		fmt.Printf("[store] ================= 警告 =================\n")
		fmt.Printf("[store] 已随机生成后台访问令牌: %s\n", cfg.Token)
		fmt.Printf("[store] 请立即登录后台在「设置」中修改，或用环境变量 TOKEN 指定\n")
		fmt.Printf("[store] =======================================\n")
	}
	return s.saveConfigLocked()
}

func (s *Store) saveConfigLocked() error {
	b, err := json.MarshalIndent(s.cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.cfgFile + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.cfgFile)
}

// Config 返回配置副本
func (s *Store) Config() model.Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return *s.cfg
}

// UpdateConfig 修改配置并落盘（Token 为空表示不修改）
func (s *Store) UpdateConfig(cfg model.Config) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cfg.Token == "" || cfg.Token == "**" {
		cfg.Token = s.cfg.Token
	}
	applyEnv(&cfg) // 环境变量始终最高优先级
	s.cfg = &cfg
	return s.saveConfigLocked()
}

// UpdateToken 单独修改令牌
func (s *Store) UpdateToken(newToken string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg.Token = newToken
	return s.saveConfigLocked()
}

// 环境变量 > config.json > 内置默认值
func applyEnv(cfg *model.Config) {
	set := func(env string, fn func(string)) {
		if v, ok := os.LookupEnv(env); ok && strings.TrimSpace(v) != "" {
			fn(strings.TrimSpace(v))
		}
	}
	seti := func(env string, fn func(int)) {
		set(env, func(v string) {
			if n, err := strconv.Atoi(v); err == nil {
				fn(n)
			}
		})
	}
	setb := func(env string, fn func(bool)) {
		set(env, func(v string) {
			fn(strings.EqualFold(v, "true") || v == "1" || strings.EqualFold(v, "yes"))
		})
	}
	set("TOKEN", func(v string) { cfg.Token = v })
	seti("PORT", func(v int) { cfg.Port = v })
	set("SUB_PATH", func(v string) {
		if !strings.HasPrefix(v, "/") {
			v = "/" + v
		}
		cfg.SubPath = v
	})
	seti("CHECK_INTERVAL", func(v int) {
		if v > 0 {
			cfg.CheckIntervalMinutes = v
		}
	})
	seti("CHECK_TIMEOUT", func(v int) {
		if v > 0 {
			cfg.CheckTimeoutSeconds = v
		}
	})
	seti("CHECK_CONCURRENCY", func(v int) {
		if v > 0 {
			cfg.CheckConcurrency = v
		}
	})
	setb("STARTUP_CHECK", func(v bool) { cfg.StartupCheck = v })
	seti("AUTO_DISABLE_FAILS", func(v int) { cfg.AutoDisableAfterFails = v })
	setb("INSECURE_TLS", func(v bool) { cfg.InsecureSkipVerify = v })
	set("USER_AGENT", func(v string) { cfg.UserAgent = v })
	setb("CHECK_SUB_URLS", func(v bool) { cfg.CheckSubURLs = v })
}

func randToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("t%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)[:n]
}

// ---------- 数据 ----------

func (s *Store) loadData() error {
	b, err := os.ReadFile(s.dataFile)
	if os.IsNotExist(err) {
		return nil // 首次启动，空数据
	}
	if err != nil {
		return err
	}
	var d model.Data
	if err := json.Unmarshal(b, &d); err != nil {
		// 数据损坏时不覆盖：改名备份后以空数据启动，避免阻塞服务也避免丢数据
		bak := s.dataFile + ".corrupt." + time.Now().Format("20060102150405")
		_ = os.Rename(s.dataFile, bak)
		fmt.Printf("[store] data.json 解析失败，已备份到 %s，以空数据启动: %v\n", bak, err)
		s.data = &model.Data{Version: 1, Sources: []model.Source{}}
		return nil
	}
	if d.Sources == nil {
		d.Sources = []model.Source{}
	}
	if d.Version == 0 {
		d.Version = 1
	}
	s.data = &d
	return nil
}

// flush 原子写入。调用方需持有写锁。
func (s *Store) flush() error {
	b, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.dataFile + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.dataFile) // 同目录 Rename 原子替换
}

// Save 显式落盘（探活攒批后调用）
func (s *Store) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.flush()
}

func NewID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return "src_" + hex.EncodeToString(b)
}

// clone 深拷贝单条源（Detail 指针也复制，避免读侧与写侧共享可变结构）
func clone(src model.Source) model.Source {
	c := src
	if src.Detail != nil {
		d := *src.Detail
		c.Detail = &d
	}
	return c
}

// List 过滤 + 排序后返回副本（绝不返回内部切片，防止 handler 序列化与探活协程竞争）
func (s *Store) List(f Filter) []model.Source {
	s.mu.RLock()
	out := make([]model.Source, 0, len(s.data.Sources))
	for i := range s.data.Sources {
		out = append(out, clone(s.data.Sources[i]))
	}
	s.mu.RUnlock()

	if f.Keyword != "" {
		kw := strings.ToLower(f.Keyword)
		out = filterSlice(out, func(x model.Source) bool {
			return strings.Contains(strings.ToLower(x.Name), kw) ||
				strings.Contains(strings.ToLower(x.URL), kw) ||
				strings.Contains(strings.ToLower(x.Remark), kw)
		})
	}
	if f.Type != "" && f.Type != "all" {
		out = filterSlice(out, func(x model.Source) bool { return x.Type == f.Type })
	}
	if f.Status != "" && f.Status != "all" {
		out = filterSlice(out, func(x model.Source) bool { return x.Status == f.Status })
	}
	if f.Group != "" && f.Group != "all" {
		out = filterSlice(out, func(x model.Source) bool { return x.Group == f.Group })
	}
	if f.Enabled == "true" || f.Enabled == "false" {
		out = filterSlice(out, func(x model.Source) bool {
			return (f.Enabled == "true") == x.Enabled
		})
	}

	sort.SliceStable(out, func(i, j int) bool {
		if f.Sort == "failFirst" {
			fi, fj := statusRank(out[i].Status), statusRank(out[j].Status)
			if fi != fj {
				return fi < fj
			}
		}
		if out[i].Sort != out[j].Sort {
			return out[i].Sort < out[j].Sort
		}
		return out[i].Name < out[j].Name
	})

	// 分页
	if f.Page > 0 && f.Size > 0 {
		start := (f.Page - 1) * f.Size
		if start >= len(out) {
			return []model.Source{}
		}
		end := start + f.Size
		if end > len(out) {
			end = len(out)
		}
		out = out[start:end]
	}
	return out
}

func statusRank(st string) int {
	switch st {
	case model.StatusFail:
		return 0
	case model.StatusOK:
		return 1
	default:
		return 2
	}
}

func filterSlice(in []model.Source, keep func(model.Source) bool) []model.Source {
	out := in[:0:0]
	for _, x := range in {
		if keep(x) {
			out = append(out, x)
		}
	}
	return out
}

// Count 按过滤条件计数（分页前端要总数）
func (s *Store) Count(f Filter) int {
	f.Page, f.Size = 0, 0
	return len(s.List(f))
}

// Get 单条副本
func (s *Store) Get(id string) (model.Source, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for i := range s.data.Sources {
		if s.data.Sources[i].ID == id {
			return clone(s.data.Sources[i]), true
		}
	}
	return model.Source{}, false
}

// GetByURL 按归一化 URL 查重
func (s *Store) GetByURL(normalizedURL string) (model.Source, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for i := range s.data.Sources {
		if s.data.Sources[i].URL == normalizedURL {
			return clone(s.data.Sources[i]), true
		}
	}
	return model.Source{}, false
}

// Add 新增（调用方负责归一化与查重）。ID 为空时生成并回填。
func (s *Store) Add(src *model.Source) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if src.ID == "" {
		src.ID = NewID()
	}
	now := time.Now()
	src.CreatedAt = now
	src.UpdatedAt = now
	if src.Group == "" {
		src.Group = "默认"
	}
	if src.Status == "" {
		src.Status = model.StatusUnknown
	}
	s.data.Sources = append(s.data.Sources, *src)
	return s.flush()
}

// Update 修改（fn 在锁内修改副本后写回）
func (s *Store) Update(id string, fn func(*model.Source)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.data.Sources {
		if s.data.Sources[i].ID == id {
			updated := clone(s.data.Sources[i])
			fn(&updated)
			updated.UpdatedAt = time.Now()
			s.data.Sources[i] = updated
			return s.flush()
		}
	}
	return fmt.Errorf("源不存在: %s", id)
}

// UpdateInMemory 探活过程先改内存不落盘
func (s *Store) UpdateInMemory(id string, fn func(*model.Source)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.data.Sources {
		if s.data.Sources[i].ID == id {
			updated := clone(s.data.Sources[i])
			fn(&updated)
			s.data.Sources[i] = updated
			return
		}
	}
}

// Delete 删除，返回实际删除数
func (s *Store) Delete(ids []string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	kept := s.data.Sources[:0]
	n := 0
	for _, x := range s.data.Sources {
		if want[x.ID] {
			n++
			continue
		}
		kept = append(kept, x)
	}
	if n > 0 {
		s.data.Sources = kept
		if err := s.flush(); err != nil {
			fmt.Printf("[store] 删除后落盘失败: %v\n", err)
		}
	}
	return n
}

// EnabledSources 订阅输出用：仅启用，按 sort、name 排序，返回副本
func (s *Store) EnabledSources() []model.Source {
	return s.List(Filter{Enabled: "true"})
}

// AllSources 全量副本
func (s *Store) AllSources() []model.Source {
	return s.List(Filter{})
}

// UpdateCheckResult 探活结果回写；persist=true 立即落盘，false 攒批由调用方统一 Save
func (s *Store) UpdateCheckResult(id string, r CheckResult, persist bool) error {
	apply := func(sp *model.Source) {
		now := time.Now()
		sp.LastCheckAt = &now
		sp.Status = r.Status
		sp.HTTPCode = r.HTTPCode
		sp.LatencyMs = r.LatencyMs
		sp.CheckMsg = r.Msg
		if r.Type != "" && r.Status == model.StatusOK {
			sp.Type = r.Type
		}
		if r.Status == model.StatusOK {
			sp.OKStreak++
			sp.FailStreak = 0
			sp.LastOKAt = &now
			if r.Detail != nil {
				sp.Detail = r.Detail // 整体替换，不原地改
			}
		} else {
			sp.FailStreak++
			sp.OKStreak = 0
		}
	}
	if persist {
		return s.Update(id, apply)
	}
	s.UpdateInMemory(id, apply)
	return nil
}

// Groups 所有分组名（去重排序）
func (s *Store) Groups() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	seen := map[string]bool{}
	for i := range s.data.Sources {
		g := s.data.Sources[i].Group
		if g == "" {
			g = "默认"
		}
		seen[g] = true
	}
	out := make([]string, 0, len(seen))
	for g := range seen {
		out = append(out, g)
	}
	sort.Strings(out)
	return out
}

// BatchAction 批量启用/禁用/删除/改组
func (s *Store) BatchAction(ids []string, action string, group string) (int, error) {
	switch action {
	case "enable", "disable":
		want := (action == "enable")
		n := 0
		for _, id := range ids {
			err := s.Update(id, func(sp *model.Source) { sp.Enabled = want })
			if err == nil {
				n++
			}
		}
		return n, nil
	case "delete":
		return s.Delete(ids), nil
	case "group":
		if group == "" {
			return 0, fmt.Errorf("改组需要 group 参数")
		}
		n := 0
		for _, id := range ids {
			err := s.Update(id, func(sp *model.Source) { sp.Group = group })
			if err == nil {
				n++
			}
		}
		return n, nil
	default:
		return 0, fmt.Errorf("未知批量操作: %s", action)
	}
}

// Stats 概览统计
func (s *Store) Stats(lastRun, nextRun string) model.Stats {
	srcs := s.List(Filter{})
	st := model.Stats{LastRunAt: lastRun, NextRunAt: nextRun}
	groups := map[string]bool{}
	for _, x := range srcs {
		st.Total++
		if x.Enabled {
			st.Enabled++
		}
		switch x.Status {
		case model.StatusOK:
			st.OK++
		case model.StatusFail:
			st.Fail++
		default:
			st.Unknown++
		}
		switch x.Type {
		case model.TypeSingle:
			st.Single++
		case model.TypeMulti:
			st.Multi++
		}
		g := x.Group
		if g == "" {
			g = "默认"
		}
		groups[g] = true
	}
	st.Groups = len(groups)
	return st
}

// ReplaceAll 导入备份 replace 模式
func (s *Store) ReplaceAll(sources []model.Source) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range sources {
		if sources[i].ID == "" {
			sources[i].ID = NewID()
		}
		if sources[i].Status == "" {
			sources[i].Status = model.StatusUnknown
		}
	}
	s.data.Sources = sources
	return s.flush()
}

// MergeAll 导入备份 merge 模式（按 URL 去重，已存在跳过）
func (s *Store) MergeAll(sources []model.Source) (added, skipped int) {
	for _, src := range sources {
		if _, exists := s.GetByURL(src.URL); exists {
			skipped++
			continue
		}
		src.ID = ""
		if err := s.Add(&src); err == nil {
			added++
		}
	}
	return added, skipped
}
