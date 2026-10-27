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
	SortBy  string // 表头排序键（优先于 Sort）：status/name/type/url/group/latency/checks/fails/time/enabled/sort
	SortDir string // 表头排序方向：asc / desc
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
	// 启动时与源列表对账一次：补齐缺失的站点源、清理孤儿，
	// 已有条目（含用户的启用状态/改名/备注/探活结果）原样保留。
	s.resyncAllFlatsLocked()
	return s, nil
}

// ---------- 配置 ----------

func (s *Store) loadConfig() error {
	cfg := model.DefaultConfig()
	var present map[string]bool // config.json 中已有的键，用于判断环境变量是否还需要提供初值

	if b, err := os.ReadFile(s.cfgFile); err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		// 首次启动：下面由环境变量填充初值
	} else {
		var raw map[string]json.RawMessage
		fromFile := model.DefaultConfig()
		e := json.Unmarshal(b, &raw)
		if e == nil {
			e = json.Unmarshal(b, &fromFile)
		}
		if e != nil {
			// 配置损坏不阻塞启动：备份后用默认值 + 环境变量重建
			bak := s.cfgFile + ".corrupt." + time.Now().Format("20060102150405")
			_ = os.Rename(s.cfgFile, bak)
			fmt.Printf("[store] config.json 解析失败，已备份到 %s，用默认值重建: %v\n", bak, e)
		} else {
			cfg = fromFile
			present = make(map[string]bool, len(raw))
			for k := range raw {
				present[k] = true
			}
		}
	}

	// 环境变量只在「配置文件里没有该项」时生效（首次启动的初始值）；
	// 之后一律以 config.json（后台「设置」页）为准，可在容器里改完配置再重启。
	applyEnv(&cfg, present)
	sanitizeConfig(&cfg)

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

// UpdateConfig 修改配置并落盘（Token 为空表示不修改）。
// 这里刻意不再套用环境变量：配置落盘后即以此为准，后台改的端口/令牌/订阅路径不会被覆盖。
func (s *Store) UpdateConfig(cfg model.Config) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cfg.Token == "" || cfg.Token == "**" {
		cfg.Token = s.cfg.Token
	}
	sanitizeConfig(&cfg)
	s.cfg = &cfg
	return s.saveConfigLocked()
}

// applyEnv 用环境变量填充配置，仅在 config.json 尚未包含该键时生效（即「首次启动的初始值」）。
// present 为配置文件里已有的键；设置 ENV_OVERRIDE=1/true 可强制让环境变量覆盖配置文件。
func applyEnv(cfg *model.Config, present map[string]bool) {
	type binding struct {
		env string
		key string // 对应的 config.json 字段名
		val string
		set func(*model.Config, string)
	}
	var bindings []binding

	add := func(env, key string, set func(*model.Config, string)) {
		v, ok := os.LookupEnv(env)
		if !ok {
			return
		}
		if v = strings.TrimSpace(v); v != "" {
			bindings = append(bindings, binding{env: env, key: key, val: v, set: set})
		}
	}
	addi := func(env, key string, set func(*model.Config, int)) {
		add(env, key, func(c *model.Config, v string) {
			if n, err := strconv.Atoi(v); err == nil {
				set(c, n)
			}
		})
	}
	addb := func(env, key string, set func(*model.Config, bool)) {
		add(env, key, func(c *model.Config, v string) { set(c, envBoolValue(v)) })
	}

	add("TOKEN", "token", func(c *model.Config, v string) { c.Token = v })
	addi("PORT", "port", func(c *model.Config, n int) { c.Port = n })
	add("SUB_PATH", "subPath", func(c *model.Config, v string) { c.SubPath = v })
	addi("CHECK_INTERVAL", "checkIntervalMinutes", func(c *model.Config, n int) {
		if n > 0 {
			c.CheckIntervalMinutes = n
		}
	})
	addi("CHECK_TIMEOUT", "checkTimeoutSeconds", func(c *model.Config, n int) {
		if n > 0 {
			c.CheckTimeoutSeconds = n
		}
	})
	addi("CHECK_CONCURRENCY", "checkConcurrency", func(c *model.Config, n int) {
		if n > 0 {
			c.CheckConcurrency = n
		}
	})
	addb("STARTUP_CHECK", "startupCheck", func(c *model.Config, b bool) { c.StartupCheck = b })
	addi("AUTO_DISABLE_FAILS", "autoDisableAfterFails", func(c *model.Config, n int) { c.AutoDisableAfterFails = n })
	addb("INSECURE_TLS", "insecureSkipVerify", func(c *model.Config, b bool) { c.InsecureSkipVerify = b })
	addb("CHECK_SUB_URLS", "checkSubUrls", func(c *model.Config, b bool) { c.CheckSubURLs = b })

	if len(bindings) == 0 {
		return
	}

	force := envBool("ENV_OVERRIDE")
	origJSON, _ := json.Marshal(*cfg)
	probe := *cfg // 探针：环境变量全量生效后的结果

	var ignored []binding
	for _, b := range bindings {
		b.set(&probe, b.val)
		if !force && present[b.key] {
			ignored = append(ignored, b) // 配置文件已有该键 → 以配置文件为准
			continue
		}
		b.set(cfg, b.val)
	}
	if len(ignored) == 0 {
		return
	}
	probeJSON, _ := json.Marshal(probe)
	if string(probeJSON) == string(origJSON) {
		return // 环境变量的值与配置文件一致，不必提示
	}

	// 逐字段比对，只列出「取值确实会被改写」的环境变量，避免同值也报警（如 Dockerfile 里的 PORT=8080）
	origM, probeM := map[string]json.RawMessage{}, map[string]json.RawMessage{}
	_ = json.Unmarshal(origJSON, &origM)
	_ = json.Unmarshal(probeJSON, &probeM)
	names := make([]string, 0, len(ignored))
	for _, b := range ignored {
		if string(origM[b.key]) != string(probeM[b.key]) {
			names = append(names, b.env)
		}
	}
	if len(names) == 0 {
		return
	}
	fmt.Printf("[store] 环境变量 %s 已设置，但 config.json 中已有对应配置，本次以配置文件为准（如需强制覆盖：ENV_OVERRIDE=1）\n",
		strings.Join(names, ", "))
}

// sanitizeConfig 兜底修正手改配置文件产生的非法值：
// 0/越界端口、空订阅路径、订阅路径带尾斜杠（会与路由模式冲突）、非法探活参数等。
func sanitizeConfig(cfg *model.Config) {
	if cfg.Port <= 0 || cfg.Port > 65535 {
		cfg.Port = 8080
	}
	sub := strings.TrimRight(strings.TrimSpace(cfg.SubPath), "/")
	if sub == "" {
		sub = "/sub"
	}
	if !strings.HasPrefix(sub, "/") {
		sub = "/" + sub
	}
	cfg.SubPath = sub
	if cfg.CheckIntervalMinutes <= 0 {
		cfg.CheckIntervalMinutes = 360
	}
	if cfg.CheckTimeoutSeconds <= 0 || cfg.CheckTimeoutSeconds > 120 {
		cfg.CheckTimeoutSeconds = 15
	}
	if cfg.CheckConcurrency <= 0 || cfg.CheckConcurrency > 100 {
		cfg.CheckConcurrency = 10
	}
}

func envBool(name string) bool {
	return envBoolValue(os.Getenv(name))
}

func envBoolValue(v string) bool {
	v = strings.TrimSpace(v)
	return strings.EqualFold(v, "true") || v == "1" || strings.EqualFold(v, "yes")
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
	// 「检测中」是只对当前进程有意义的瞬态：上次进程中途退出（崩溃/被 kill）会把
	// 这个状态永久留在文件里，概览就一直显示「检测中」。加载时统一归位为「未检测」。
	for i := range d.Sources {
		if d.Sources[i].Status == model.StatusChecking {
			d.Sources[i].Status = model.StatusUnknown
		}
	}
	for i := range d.Flats {
		if d.Flats[i].Status == model.StatusChecking {
			d.Flats[i].Status = model.StatusUnknown
		}
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

	if f.SortBy != "" {
		// 表头排序优先
		sortByHeader(out, func(a, b model.Source) int { return cmpSource(a, b, f.SortBy) }, f.SortDir == "desc", sourceFallback)
	} else {
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
			if out[i].Name != out[j].Name {
				return out[i].Name < out[j].Name
			}
			return out[i].URL < out[j].URL // 兜底定序：同名同排序值时保证跨请求顺序稳定，翻页不跳动
		})
	}

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
	if src.Type == "" {
		src.Type = model.TypeUnknown // 空类型等价于「还没探活，不知道是单仓还是多仓」
	}
	s.data.Sources = append(s.data.Sources, *src)
	s.syncFlatsForSourceLocked(*src) // 同步生成站点源
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
			s.syncFlatsForSourceLocked(updated) // 名称/分组/地址/内容变化同步到站点源
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
		for _, id := range ids {
			s.removeSourceFlatRefsLocked(id) // 摘除站点源上的父引用，无父则删
		}
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

// UpdateCheckResult 探活结果回写；persist=true 立即落盘，false 攒批由调用方统一 Save。
// 多仓探活成功后同步展开子仓到站点源。
func (s *Store) UpdateCheckResult(id string, r CheckResult, persist bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	updated := s.updateCheckResultLocked(id, r)
	if updated && persist {
		return s.flush()
	}
	return nil
}

func (s *Store) updateCheckResultLocked(id string, r CheckResult) bool {
	for i := range s.data.Sources {
		if s.data.Sources[i].ID != id {
			continue
		}
		sp := clone(s.data.Sources[i])
		now := time.Now()
		sp.LastCheckAt = &now
		sp.Status = r.Status
		sp.HTTPCode = r.HTTPCode
		sp.LatencyMs = r.LatencyMs
		sp.CheckMsg = r.Msg
		sp.CheckCount++ // 累计检测次数
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
			sp.FailCount++ // 累计检测不通次数
		}
		s.data.Sources[i] = sp
		s.syncFlatsForSourceLocked(sp) // 多仓子仓列表变化同步到站点源
		return true
	}
	return false
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
	groups := map[string]int{}
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
		case model.StatusChecking:
			st.Checking++ // 检测中是瞬态，别混进 unknown
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
		groups[g]++
	}
	st.Groups = len(groups)
	st.GroupDist = make([]model.GroupCount, 0, len(groups))
	for name, n := range groups {
		st.GroupDist = append(st.GroupDist, model.GroupCount{Name: name, Count: n})
	}
	// 数量降序、同数量按名称升序，前端可以直接取前 N 个画条形图
	sort.Slice(st.GroupDist, func(i, j int) bool {
		if st.GroupDist[i].Count != st.GroupDist[j].Count {
			return st.GroupDist[i].Count > st.GroupDist[j].Count
		}
		return st.GroupDist[i].Name < st.GroupDist[j].Name
	})

	// 站点源（订阅实际输出层）口径：概览要能看出真正会下发的条数
	for _, f := range s.ListFlats(FlatFilter{}) {
		st.FlatTotal++
		if f.EffectiveEnabled {
			st.FlatEnabled++
		}
		switch f.Status {
		case model.StatusOK:
			st.FlatOK++
		case model.StatusFail:
			st.FlatFail++
		case model.StatusChecking:
			st.FlatChecking++
		default:
			st.FlatUnknown++
		}
	}
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
		if sources[i].Type == "" {
			sources[i].Type = model.TypeUnknown
		}
	}
	s.data.Sources = sources
	s.resyncAllFlatsLocked() // 全量重建站点源
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
