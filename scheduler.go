package main

import (
	"errors"
	"time"
)

func validateRule(r ServiceRule) error {
	if _, e := time.LoadLocation(r.Zone); e != nil {
		return errors.New("请选择有效时区")
	}
	if _, e := time.Parse("15:04", r.Start); e != nil {
		return errors.New("开始时间格式应为 HH:MM")
	}
	if _, e := time.Parse("15:04", r.End); e != nil {
		return errors.New("结束时间格式应为 HH:MM")
	}
	if r.Start == r.End {
		return errors.New("开始与结束时间不能相同")
	}
	if len(r.Days) == 0 {
		return errors.New("至少选择一天")
	}
	seen := map[int]bool{}
	for _, d := range r.Days {
		if d < 0 || d > 6 || seen[d] {
			return errors.New("星期设置无效")
		}
		seen[d] = true
	}
	return nil
}

// Window belongs to the start date, including cross-midnight intervals.
func ruleWindow(r ServiceRule, now time.Time) (key string, active bool, end time.Time) {
	if !r.Enabled || validateRule(r) != nil {
		return
	}
	loc, _ := time.LoadLocation(r.Zone)
	n := now.In(loc)
	for offset := -1; offset <= 0; offset++ {
		day := n.AddDate(0, 0, offset)
		allowed := false
		for _, d := range r.Days {
			if int(day.Weekday()) == d {
				allowed = true
			}
		}
		if !allowed {
			continue
		}
		date := day.Format("2006-01-02")
		st, _ := time.ParseInLocation("2006-01-02 15:04", date+" "+r.Start, loc)
		en, _ := time.ParseInLocation("2006-01-02 15:04", date+" "+r.End, loc)
		if !en.After(st) {
			en = en.AddDate(0, 0, 1)
		}
		if !now.Before(st) && now.Before(en) {
			return date + "/" + r.Start + "/" + r.Zone, true, en
		}
	}
	return
}
func nextWindow(r ServiceRule, now time.Time) string {
	if !r.Enabled || validateRule(r) != nil {
		return "未启用"
	}
	if _, active, end := ruleWindow(r, now); active {
		return "计划结束 " + end.Format("01-02 15:04 MST")
	}
	loc, _ := time.LoadLocation(r.Zone)
	for i := 0; i < 8; i++ {
		d := now.In(loc).AddDate(0, 0, i)
		for _, w := range r.Days {
			if int(d.Weekday()) == w {
				st, _ := time.ParseInLocation("2006-01-02 15:04", d.Format("2006-01-02")+" "+r.Start, loc)
				if st.After(now) {
					return "下次启动 " + st.Format("01-02 15:04 MST")
				}
			}
		}
	}
	return "未安排"
}
func (s *Studio) SaveServiceRule(r ServiceRule, confirmed bool) error {
	if e := validateRule(r); e != nil {
		return e
	}
	if r.Enabled && !confirmed {
		return errors.New("请确认规则会自动创建计费实例，并在任务完成和 Drive 保存后释放")
	}
	m := s.services()
	if m.err != nil {
		return m.err
	}
	m.op.Lock()
	defer m.op.Unlock()
	v := m.get()
	p, e := m.identity(v)
	if e != nil {
		return e
	}
	if v.ID == "" {
		v.ID = random()
		v.Account = p.Email
		v.Binding = accountBinding(p.Email)
	}
	v.Rule = r
	v.LastWindow = ""
	if key, active, _ := ruleWindow(r, time.Now()); active {
		v.LastWindow = key
		if v.Runtime != "" {
			v.RunningWindow = key
		}
	}
	v.Message = "规则已保存；从下一个开始时间执行，不补开当前时段"
	return m.put(v)
}
func (m *serviceManager) loop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	previous := time.Now()
	if m.err == nil {
		m.op.Lock()
		r := m.get()
		if key, active, _ := ruleWindow(r.Rule, previous); active && r.Runtime == "" && r.LastWindow != key {
			r.LastWindow = key
			r.Message = "应用重新启动，已跳过错过的启动时段"
			_ = m.put(r)
		}
		m.op.Unlock()
	}
	for range ticker.C {
		now := time.Now()
		if m.err != nil || !m.op.TryLock() {
			previous = now
			continue
		}
		m.tick(now, previous)
		previous = now
		m.op.Unlock()
	}
}
func (m *serviceManager) tick(now, previous time.Time) {
	r := m.get()
	if r.ID == "" {
		return
	}
	if _, e := m.identity(r); e != nil {
		if r.Runtime != "" {
			r.State = "disconnected"
		}
		_ = m.fail(r, e)
		return
	}
	key, active, _ := ruleWindow(r.Rule, now)
	if r.Runtime != "" {
		if gone, e := m.reconcileGone(&r); e != nil {
			r.Message = e.Error()
			_ = m.put(r)
			return
		} else if gone {
			_ = m.put(r)
			return
		}
		_ = m.keepAlive(r)
		if e := m.inspect(&r); e != nil {
			r.State = "disconnected"
			r.Message = e.Error()
			_ = m.put(r)
			return
		}
		if !r.StopRequested {
			if m.ka != nil {
				m.ka.notify()
			}
			if r.RemoteRoot != "" {
				// Repairs the cloud keep-alive if its kernel died; retried next tick on failure.
				if p, e := m.studio.proxy(r.Runtime); e == nil {
					_ = ensureKeepAlive(p, r.RemoteRoot)
				}
			}
		}
		if active && r.Rule.Enabled && r.RunningWindow == "" {
			r.RunningWindow = key
		}
		if r.RunningWindow != "" && r.Rule.Enabled && (!active || key != r.RunningWindow) {
			r.StopRequested = true
		}
		if r.StopRequested {
			_ = m.finishStop(&r)
		} else {
			_ = m.put(r)
		}
		return
	}
	if active && key != r.LastWindow && key != r.Suppressed {
		r.LastWindow = key
		if now.Sub(previous) > 90*time.Second {
			r.Message = "Mac 曾暂停运行，已跳过错过的启动时段"
			_ = m.put(r)
			return
		}
		if e := m.put(r); e == nil {
			_ = m.start(true)
		}
	}
}
func (s *Studio) StopToybox(confirmed bool) error {
	if !confirmed {
		return errors.New("请确认等待任务、保存到 Drive 后释放此服务实例")
	}
	m := s.services()
	if m.err != nil {
		return m.err
	}
	if !m.op.TryLock() {
		return errors.New("服务操作正在进行，请稍后重试")
	}
	defer m.op.Unlock()
	r := m.get()
	if r.Runtime == "" {
		return errors.New("没有已确认的服务实例可释放")
	}
	if _, e := m.identity(r); e != nil {
		return e
	}
	r.StopRequested = true
	if key, active, _ := ruleWindow(r.Rule, time.Now()); active {
		r.Suppressed = key
	}
	if r.RemoteRoot == "" {
		return m.releaseUninstalled(&r)
	}
	r.Message = "正在停止接收新任务，等待现有任务完成"
	if e := m.put(r); e != nil {
		return e
	}
	if e := m.command(r, "drain", random()); e != nil {
		return e
	}
	go s.nudgeStopProgress()
	return nil
}

func (s *Studio) nudgeStopProgress() {
	for _, wait := range []time.Duration{2 * time.Second, 4 * time.Second} {
		time.Sleep(wait)
		m := s.svc
		if m == nil {
			return
		}
		r := m.get()
		if r.Runtime == "" || !r.StopRequested {
			return
		}
		_ = s.RefreshService()
	}
}

func (s *Studio) forceReleaseToybox() error {
	m := s.services()
	if m.err != nil {
		return m.err
	}
	if !m.op.TryLock() {
		return errors.New("服务操作正在进行，请稍后重试")
	}
	defer m.op.Unlock()
	r := m.get()
	if r.Runtime == "" {
		return errors.New("没有已确认的服务实例可释放")
	}
	if _, e := m.identity(r); e != nil {
		return e
	}
	r.StopRequested = true
	if key, active, _ := ruleWindow(r.Rule, time.Now()); active {
		r.Suppressed = key
	}
	r.State = "releasing"
	r.Message = "正在强制释放实例（未执行 Drive 备份）"
	if e := m.put(r); e != nil {
		return e
	}
	if e := s.Release(r.Runtime, true); e != nil {
		return m.fail(r, e)
	}
	r.Runtime = ""
	r.RunningWindow = ""
	r.Operation = ""
	r.PostSent = false
	r.StopRequested = false
	r.BackupNonce = ""
	r.RemoteRoot = ""
	r.RemoteVersion = ""
	r.RemoteOutdated = false
	r.URL = ""
	r.State = "stopped"
	r.Message = "已强制释放实例（未执行 Drive 备份）"
	return m.put(r)
}

func (s *Studio) RefreshService() error {
	m := s.services()
	if m.err != nil {
		return m.err
	}
	if !m.op.TryLock() {
		return nil
	}
	defer m.op.Unlock()
	r := m.get()
	if r.Runtime == "" {
		return nil
	}
	if _, e := m.identity(r); e != nil {
		return m.fail(r, e)
	}
	if e := m.inspect(&r); e != nil {
		r.State = "disconnected"
		return m.fail(r, e)
	}
	if r.StopRequested {
		return m.finishStop(&r)
	}
	return m.put(r)
}

func (s *Studio) openToyboxURL() (string, error) {
	m := s.services()
	r := m.get()
	if _, e := m.identity(r); e != nil {
		return "", e
	}
	security, e := s.ToyboxAccessStatus()
	if e != nil {
		return "", e
	}
	if !security.Protected {
		return "", errors.New("请先启用网页鉴权，或等待网页进程确认后再打开")
	}
	state, e := m.state(r)
	if e != nil {
		return "", e
	}
	readiness := readinessFromState(state, time.Now(), r.TunnelURL)
	if !readiness.Fresh || !readiness.Health || state.Stage != "online" {
		return "", errors.New("服务尚未确认就绪，请查看服务状态后再打开网页")
	}
	return urlForToyboxWithCustom(state.URL, r.TunnelURL)
}

func (s *Studio) OpenToybox() error {
	u, e := s.openToyboxURL()
	if e != nil {
		return e
	}
	return openToyboxBrowser(u)
}
