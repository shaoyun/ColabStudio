package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"
)

type ResourceSample struct {
	Time      float64  `json:"time"`
	CPU       *float64 `json:"cpu"`
	GPU       *float64 `json:"gpu"`
	RAM       *float64 `json:"ram"`
	VRAM      *float64 `json:"vram"`
	RAMUsed   *float64 `json:"ramUsed,omitempty"`
	RAMTotal  *float64 `json:"ramTotal,omitempty"`
	VRAMUsed  *float64 `json:"vramUsed,omitempty"`
	VRAMTotal *float64 `json:"vramTotal,omitempty"`
	CPUModel  string   `json:"cpuModel,omitempty"`
	Cores     int      `json:"cores,omitempty"`
	GPUName   string   `json:"gpuName,omitempty"`
}
type ResourceHistory struct {
	Managed bool             `json:"managed"`
	Owner   string           `json:"owner"`
	Binding string           `json:"binding"`
	Samples []ResourceSample `json:"samples"`
	Message string           `json:"message"`
}

func validSamples(h ResourceHistory, r ServiceRecord, now time.Time) ([]ResourceSample, error) {
	if h.Owner != r.ID || h.Binding != r.Binding {
		return nil, errors.New("资源采样身份不匹配")
	}
	out := make([]ResourceSample, 0, 60)
	for _, v := range h.Samples {
		age := float64(now.Unix()) - v.Time
		if math.IsNaN(v.Time) || math.IsInf(v.Time, 0) || age < -10 || age > 900 {
			continue
		}
		if len(out) > 0 && v.Time <= out[len(out)-1].Time {
			continue
		}
		for _, p := range []**float64{&v.CPU, &v.GPU, &v.RAM, &v.VRAM} {
			if *p != nil && (math.IsNaN(**p) || math.IsInf(**p, 0) || **p < 0 || **p > 100) {
				*p = nil
			}
		}
		out = append(out, v)
	}
	if len(out) > 60 {
		out = out[len(out)-60:]
	}
	return out, nil
}

// ResourceHistory installs one detached sampler only for this app's owned VM.
// Subsequent polls read a small JSON file; they never execute notebook cells.
func (s *Studio) ResourceHistory(name string) (ResourceHistory, error) {
	var h ResourceHistory
	m := s.services()
	if m.err != nil {
		return h, m.err
	}
	if !m.op.TryLock() {
		return h, errors.New("服务操作进行中，稍后自动刷新资源")
	}
	defer m.op.Unlock()
	r := m.get()
	if r.Runtime != name || r.ID == "" || r.Binding == "" {
		h.Message = "此实例未由本应用管理；显示只读资源，CPU 曲线暂不可用。"
		return h, nil
	}
	if !kernelID.MatchString(r.ID) {
		return h, errors.New("服务标识无效，未安装资源采样器")
	}
	h.Managed = true
	if r.StopRequested {
		return h, errors.New("服务正在停止，已暂停资源采样初始化")
	}
	if _, e := m.identity(r); e != nil {
		return h, e
	}
	p, e := s.proxy(name)
	if e != nil {
		return h, e
	}
	roots := []string{"content/.colabstudio-monitor/" + r.ID, ".colabstudio-monitor/" + r.ID}
	for _, root := range roots {
		var v ResourceHistory
		if remoteJSON(p, root, "metrics.json", &v) == nil {
			samples, err := validSamples(v, r, time.Now())
			if err != nil {
				return h, err
			}
			if len(samples) > 0 {
				h.Samples = samples
				if float64(time.Now().Unix())-samples[len(samples)-1].Time < 45 {
					return h, nil
				}
			}
		}
	}
	// Failed starts are cooled down: do not repeatedly create temporary kernels.
	m.mu.Lock()
	last := m.metricsAttempt
	runtime := m.metricsRuntime
	if runtime == name && time.Since(last) < time.Minute {
		m.mu.Unlock()
		h.Message = "正在等待远端采样器，每 15 秒更新。"
		return h, nil
	}
	m.metricsAttempt = time.Now()
	m.metricsRuntime = name
	m.mu.Unlock()
	source, _ := templates.ReadFile("templates/studio_metrics.py")
	payload, _ := json.Marshal(base64.StdEncoding.EncodeToString(source))
	owner, _ := json.Marshal(r.ID)
	binding, _ := json.Marshal(r.Binding)
	code := fmt.Sprintf(`import pathlib,base64,subprocess,sys,os
owner=%s
binding=%s
root=pathlib.Path('/content/.colabstudio-monitor')/owner
root.mkdir(parents=True,exist_ok=True,mode=0o700)
p=root/'sampler.py'
p.write_bytes(base64.b64decode(%s))
os.chmod(p,0o600)
with open(os.devnull,'wb') as out:
 subprocess.Popen([sys.executable,str(p),str(root),owner,binding],stdin=subprocess.DEVNULL,stdout=out,stderr=out,start_new_session=True)
`, string(owner), string(binding), string(payload))
	if e = executeOwned(p, code); e != nil {
		return h, e
	}
	h.Message = "已启用轻量采样；首个 CPU 利用率需等待 15 秒。"
	return h, nil
}
