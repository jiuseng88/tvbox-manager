package scheduler

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"tvbox-manager/internal/checker"
	"tvbox-manager/internal/model"
	"tvbox-manager/internal/store"
)

// Scheduler 定时探活调度：定时全量、手动触发、进度上报、自动禁用、子仓递归探活。
type Scheduler struct {
	st *store.Store

	cfgMu  sync.RWMutex
	cfg    *model.Config
	ck     *checker.Checker
	client func() *checker.Checker // 供外部取当前 checker（预览用）

	mu      sync.Mutex
	running bool

	timer *time.Timer
	stop  chan struct{}
	done  chan struct{}

	progMu sync.RWMutex
	prog   model.CheckProgress
}

// Start 创建并启动调度器
func Start(st *store.Store, cfg *model.Config) *Scheduler {
	s := &Scheduler{
		st:   st,
		cfg:  cfg,
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
	s.ck = checker.New(cfg)
	s.client = func() *checker.Checker { return s.ck }
	interval := s.interval()
	s.timer = time.NewTimer(interval)
	s.setNextRun(time.Now().Add(interval))
	go s.loop()
	if cfg.StartupCheck {
		go func() {
			log.Printf("[scheduler] startupCheck=true，启动即全量探活")
			s.RunAll("scheduler")
		}()
	}
	return s
}

// Checker 当前探活器（预览接口复用其 HTTP 客户端与超时配置）
func (s *Scheduler) Checker() *checker.Checker {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.ck
}

func (s *Scheduler) interval() time.Duration {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	mins := s.cfg.CheckIntervalMinutes
	if mins <= 0 {
		mins = 360
	}
	return time.Duration(mins) * time.Minute
}

func (s *Scheduler) loop() {
	defer close(s.done)
	timer := s.timer // 本地引用，避免与 Shutdown/Reload 竞态
	for {
		select {
		case <-timer.C:
			s.RunAll("scheduler")
			s.mu.Lock()
			timer.Reset(s.interval())
			s.mu.Unlock()
			s.setNextRun(time.Now().Add(s.interval()))
		case <-s.stop:
			timer.Stop()
			return
		}
	}
}

// Reload 配置变更后重设间隔与探活器，无需重启
func (s *Scheduler) Reload(cfg *model.Config) {
	s.cfgMu.Lock()
	s.cfg = cfg
	s.ck = checker.New(cfg)
	s.cfgMu.Unlock()
	s.mu.Lock()
	if s.timer != nil && !s.timer.Stop() {
		select {
		case <-s.timer.C:
		default:
		}
	}
	s.timer.Reset(s.interval())
	s.mu.Unlock()
	s.setNextRun(time.Now().Add(s.interval()))
}

// RunManual 手动触发；已有任务在跑返回 409 语义错误与当前进度
func (s *Scheduler) RunManual() error {
	if !s.tryStart() {
		return fmt.Errorf("已有探活任务在运行")
	}
	go s.runAll("manual")
	return nil
}

// RunAll 同步或异步触发的统一入口（外部直接调用时同步执行）
func (s *Scheduler) RunAll(trigger string) {
	if !s.tryStart() {
		log.Printf("[scheduler] 已有探活任务在运行，跳过本次")
		return
	}
	s.runAll(trigger)
}

func (s *Scheduler) tryStart() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		return false
	}
	s.running = true
	return true
}

func (s *Scheduler) finish() {
	s.mu.Lock()
	s.running = false
	s.mu.Unlock()
}

// Running 当前是否有任务
func (s *Scheduler) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

func (s *Scheduler) setNextRun(t time.Time) {
	s.progMu.Lock()
	s.prog.NextRunAt = &t
	s.progMu.Unlock()
}

func (s *Scheduler) runAll(trigger string) {
	defer s.finish()

	sources := s.st.AllSources()
	now := time.Now()
	s.progMu.Lock()
	s.prog = model.CheckProgress{
		Running:   true,
		Total:     len(sources),
		Done:      0,
		OK:        0,
		Fail:      0,
		StartedAt: &now,
		NextRunAt: s.prog.NextRunAt,
		TriggerBy: trigger,
	}
	s.progMu.Unlock()

	if len(sources) == 0 {
		end := time.Now()
		s.progMu.Lock()
		s.prog.Running = false
		s.prog.LastRunAt = &end
		s.progMu.Unlock()
		return
	}

	cfgSnap := s.currentCfg()
	ctx := context.Background()
	sem := make(chan struct{}, cfgSnap.CheckConcurrency)
	var wg sync.WaitGroup
	var persistMu sync.Mutex
	persistCount := 0

	for i := range sources {
		src := sources[i]
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[scheduler] 探活协程 panic（已隔离）: %v", r)
				}
			}()

			res, _ := s.Checker().Fetch(ctx, src.URL, false)
			_ = s.st.UpdateCheckResult(src.ID, store.CheckResult{
				Status: res.Status, HTTPCode: res.HTTPCode, LatencyMs: res.LatencyMs,
				Msg: res.Msg, Type: res.Type, Detail: res.Detail,
			}, false)

			// 连续失败自动禁用
			if res.Status == model.StatusFail && cfgSnap.AutoDisableAfterFails > 0 {
				s.st.UpdateInMemory(src.ID, func(sp *model.Source) {
					if sp.FailStreak >= cfgSnap.AutoDisableAfterFails && sp.Enabled {
						sp.Enabled = false
						sp.CheckMsg += "；已自动禁用"
					}
				})
			}

			// 多仓子仓递归探活（可选）
			if cfgSnap.CheckSubURLs && res.Status == model.StatusOK && res.Detail != nil && len(res.Detail.SubURLs) > 0 {
				s.probeSubURLs(src.ID, res.Detail.SubURLs)
			}

			s.progMu.Lock()
			s.prog.Done++
			if res.Status == model.StatusOK {
				s.prog.OK++
			} else {
				s.prog.Fail++
			}
			s.progMu.Unlock()

			// 批量落盘：每 20 个一次，避免高频 IO
			persistMu.Lock()
			persistCount++
			need := persistCount%20 == 0
			persistMu.Unlock()
			if need {
				if err := s.st.Save(); err != nil {
					log.Printf("[scheduler] 中途落盘失败: %v", err)
				}
			}
		}()
	}
	wg.Wait()

	if err := s.st.Save(); err != nil {
		log.Printf("[scheduler] 写入数据失败: %v", err)
	}
	end := time.Now()
	s.progMu.Lock()
	s.prog.Running = false
	s.prog.LastRunAt = &end
	s.progMu.Unlock()
	log.Printf("[scheduler] 探活完成: %d 源, %d 可用, %d 失败 (触发: %s)",
		s.prog.Total, s.prog.OK, s.prog.Fail, trigger)
}

// probeSubURLs 多仓子仓探活（并发减半），结果回写 Detail.SubURLs[].Status
func (s *Scheduler) probeSubURLs(sourceID string, subs []model.SubURL) {
	cfgSnap := s.currentCfg()
	conc := cfgSnap.CheckConcurrency / 2
	if conc < 1 {
		conc = 1
	}
	sem := make(chan struct{}, conc)
	var wg sync.WaitGroup
	statuses := make([]string, len(subs))
	for i := range subs {
		i := i
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			res, _ := s.Checker().Fetch(context.Background(), subs[i].URL, false)
			if res.Status == model.StatusOK {
				statuses[i] = model.StatusOK
			} else {
				statuses[i] = "fail: " + res.Msg
			}
		}()
	}
	wg.Wait()
	s.st.UpdateInMemory(sourceID, func(sp *model.Source) {
		if sp.Detail == nil {
			return
		}
		d := *sp.Detail
		if len(d.SubURLs) == len(statuses) {
			copied := make([]model.SubURL, len(d.SubURLs))
			copy(copied, d.SubURLs)
			for i := range copied {
				copied[i].Status = statuses[i]
			}
			d.SubURLs = copied
		}
		sp.Detail = &d
	})
}

func (s *Scheduler) currentCfg() *model.Config {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	cfg := *s.cfg
	return &cfg
}

// Progress 进度快照
func (s *Scheduler) Progress() model.CheckProgress {
	s.progMu.RLock()
	defer s.progMu.RUnlock()
	return s.prog
}

// Shutdown 优雅停止：停定时器，等待进行中的探活最多 10s
func (s *Scheduler) Shutdown() {
	close(s.stop)
	select {
	case <-s.done:
	case <-time.After(10 * time.Second):
	}
	// 探活结束后统一落盘一次，保证结果不丢
	_ = s.st.Save()
}
