package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"golang.org/x/net/websocket"
)

var cpuTrace = func(string) {}

type Usage struct {
	Balance     *float64 `json:"currentBalance"`
	Rate        *float64 `json:"consumptionRateHourly"`
	Assignments *int     `json:"assignmentsCount"`
}
type Memory struct {
	Total *float64 `json:"totalBytes"`
	Free  *float64 `json:"freeBytes"`
}
type GPU struct {
	Name        string   `json:"name"`
	Used        *float64 `json:"memoryUsedBytes"`
	Total       *float64 `json:"memoryTotalBytes"`
	Utilization *float64 `json:"gpuUtilization"`
}
type Disk struct {
	Filesystem struct {
		Label string   `json:"label"`
		Total *float64 `json:"totalBytes"`
		Used  *float64 `json:"usedBytes"`
	} `json:"filesystem"`
}
type Resources struct {
	Memory Memory `json:"memory"`
	GPUs   []GPU  `json:"gpus"`
	Disks  []Disk `json:"disks"`
}
type CPUInfo struct {
	Model       string   `json:"model"`
	Cores       int      `json:"cores"`
	Utilization *float64 `json:"utilization"`
}
type runtimeProxy struct {
	URL   string `json:"url"`
	Token string `json:"token"`
}

func (s *Studio) Usage() (Usage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var u Usage
	t, e := access()
	if e != nil {
		return u, e
	}
	e = tunnelRequest(httpClient, tunnelBase, t, "GET", "ccu-info?authuser=0", "", &u)
	return u, e
}
func (s *Studio) proxy(name string) (runtimeProxy, error) {
	t, e := access()
	if e != nil {
		return runtimeProxy{}, e
	}
	if strings.HasPrefix(name, "compat/") {
		endpoint, e := decodeEndpoint(name)
		if e != nil {
			return runtimeProxy{}, e
		}
		var data struct {
			Assignments []assignment `json:"assignments"`
		}
		if e = tunnelRequest(httpClient, tunnelBase, t, "GET", "assignments?authuser=0", "", &data); e != nil {
			return runtimeProxy{}, e
		}
		for _, a := range data.Assignments {
			if a.Endpoint == endpoint {
				return runtimeProxy{a.Proxy.URL, a.Proxy.Token}, nil
			}
		}
		return runtimeProxy{}, errors.New("实例已不存在，请刷新")
	}
	if !runtimeName.MatchString(name) {
		return runtimeProxy{}, errors.New("无效实例")
	}
	var r struct {
		Info runtimeProxy `json:"connectionInfo"`
	}
	if e = s.call("GET", name, nil, &r); e != nil {
		return runtimeProxy{}, e
	}
	return r.Info, nil
}
func proxyURL(p runtimeProxy, path string) (string, error) {
	u, e := url.Parse(p.URL)
	if e != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" || p.Token == "" {
		return "", errors.New("实例代理连接信息无效")
	}
	h := u.Hostname()
	if !(strings.HasSuffix(h, ".googleusercontent.com") || strings.HasSuffix(h, ".r.colab.dev")) {
		return "", errors.New("实例代理域名不受信任")
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/" + path
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}
func proxyRequest(client *http.Client, p runtimeProxy, method, path string, body any, out any) error {
	address, e := proxyURL(p, path)
	if e != nil {
		return e
	}
	var b bytes.Buffer
	if body != nil {
		json.NewEncoder(&b).Encode(body)
	}
	req, e := http.NewRequestWithContext(context.Background(), method, address, &b)
	if e != nil {
		return errors.New("无法构造实例资源请求")
	}
	req.Header.Set("X-Colab-Runtime-Proxy-Token", p.Token)
	req.Header.Set("Content-Type", "application/json")
	// Credentials must never follow a redirect to another host.
	safe := *client
	safe.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, e := safe.Do(req)
	if e != nil {
		return errors.New("实例资源暂时无法连接，请刷新重试")
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("实例资源 HTTP %d，实例可能已断开或暂不可用", resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if readErr != nil {
		return errors.New("实例资源读取失败")
	}
	data = bytes.TrimSpace(data)
	data = bytes.TrimPrefix(data, []byte(")]}'"))
	if e = json.Unmarshal(data, out); e != nil {
		return errors.New("实例资源返回格式无效")
	}
	return nil
}
func (s *Studio) Resources(name string) (Resources, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var r Resources
	p, e := s.proxy(name)
	if e != nil {
		return r, e
	}
	e = proxyRequest(httpClient, p, "GET", "api/colab/resources", nil, &r)
	return r, e
}

const cpuDiagnostic = `import os, json, time
model = ""
try:
    for line in open("/proc/cpuinfo"):
        if line.startswith("model name"):
            model = line.split(":", 1)[1].strip()
            break
except OSError:
    pass
def cpu_sample():
    values = list(map(int, open("/proc/stat").readline().split()[1:9]))
    return sum(values), values[3] + (values[4] if len(values)>4 else 0)
usage = None
try:
    total1, idle1 = cpu_sample()
    time.sleep(0.2)
    total2, idle2 = cpu_sample()
    if total2 > total1:
        usage = max(0, min(100, 100 * (1 - (idle2-idle1)/(total2-total1))))
except (OSError, ValueError):
    pass
print(json.dumps({"model": model, "cores": os.cpu_count() or 0, "utilization": usage}))
`

var kernelID = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// CPU runs only on explicit user request. It uses an idle kernel with an isolated
// namespace, or creates a temporary kernel only when no kernels exist. It never
// deletes a pre-existing kernel or creates a new runtime.
func (s *Studio) CPU(name string) (info CPUInfo, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, e := s.proxy(name)
	if e != nil {
		return info, e
	}
	var kernels []diagnosticKernel
	if e = proxyRequest(httpClient, p, "GET", "api/kernels", nil, &kernels); e != nil {
		return info, e
	}
	selected, e := selectIdleKernel(kernels)
	if e != nil {
		return info, e
	}
	owned := selected == ""
	k := diagnosticKernel{ID: selected}
	if owned {
		if e = proxyRequest(httpClient, p, "POST", "api/kernels", map[string]string{"name": "python3"}, &k); e != nil {
			return info, e
		}
	}
	if !kernelID.MatchString(k.ID) {
		return info, errors.New("诊断 kernel 标识无效")
	}
	if owned {
		defer func() {
			if e := proxyRequest(httpClient, p, "DELETE", "api/kernels/"+k.ID, nil, nil); e != nil {
				err = errors.New("CPU 诊断结束，但临时 kernel 清理未确认；请在 Colab 检查运行中的 kernel")
			}
		}()
	}
	session := random()
	config, e := diagnosticSocketConfig(p, k.ID, session)
	if e != nil {
		return info, e
	}
	ws, e := websocket.DialConfig(config)
	if e != nil {
		return info, errors.New("CPU 诊断通道连接失败")
	}
	defer ws.Close()
	ws.MaxPayloadBytes = 64 << 10
	ws.SetDeadline(time.Now().Add(40 * time.Second))
	id := random()
	quotedCode, _ := json.Marshal(cpuDiagnostic)
	isolatedCode := "exec(" + string(quotedCode) + ", {})"
	message := map[string]any{"header": map[string]any{"msg_id": id, "username": "colabstudio", "session": session, "date": time.Now().UTC().Format(time.RFC3339Nano), "msg_type": "execute_request", "version": "5.3"}, "parent_header": map[string]any{}, "metadata": map[string]any{}, "channel": "shell", "content": map[string]any{"code": isolatedCode, "silent": true, "store_history": false, "user_expressions": map[string]any{}, "allow_stdin": false, "stop_on_error": true}, "buffers": []any{}}
	cpuTrace("execute_request sent")
	if e = websocket.JSON.Send(ws, message); e != nil {
		return info, errors.New("无法提交只读 CPU 诊断")
	}
	output := ""
	for i := 0; i < 200; i++ {
		var reply struct {
			Parent struct {
				ID string `json:"msg_id"`
			} `json:"parent_header"`
			Type   string `json:"msg_type"`
			Header struct {
				Type string `json:"msg_type"`
			} `json:"header"`
			Content struct {
				Text   string `json:"text"`
				Status string `json:"execution_state"`
			} `json:"content"`
		}
		if e = websocket.JSON.Receive(ws, &reply); e != nil {
			cpuTrace(fmt.Sprintf("receive error type: %T", e))
			return info, errors.New("CPU 诊断超时或连接中断")
		}
		cpuTrace("message: " + reply.Header.Type + " parent matches: " + fmt.Sprint(reply.Parent.ID == id))
		if reply.Parent.ID != id {
			continue
		}
		kind := reply.Header.Type
		if kind == "" {
			kind = reply.Type
		}
		if kind == "stream" {
			output += reply.Content.Text
			if len(output) > 65536 {
				return info, errors.New("CPU 诊断返回过大")
			}
		}
		if kind == "error" {
			return info, errors.New("CPU 诊断执行失败")
		}
		if kind == "status" && reply.Content.Status == "idle" {
			if json.Unmarshal([]byte(strings.TrimSpace(output)), &info) != nil {
				return info, errors.New("CPU 诊断未返回有效结果")
			}
			return info, nil
		}
	}
	return info, errors.New("CPU 诊断没有完成")
}

type diagnosticKernel struct {
	ID    string `json:"id"`
	State string `json:"execution_state"`
}

func selectIdleKernel(kernels []diagnosticKernel) (string, error) {
	if len(kernels) == 0 {
		return "", nil
	}
	for _, k := range kernels {
		if k.State == "idle" && kernelID.MatchString(k.ID) {
			return k.ID, nil
		}
	}
	return "", errors.New("实例 kernel 正在忙碌或尚未就绪，未提交 CPU 诊断；请空闲后重试")
}

func diagnosticSocketConfig(p runtimeProxy, id, session string) (*websocket.Config, error) {
	if !kernelID.MatchString(id) {
		return nil, errors.New("诊断 kernel 标识无效")
	}
	address, e := proxyURL(p, "api/kernels/"+id+"/channels")
	if e != nil {
		return nil, e
	}
	address = "wss" + strings.TrimPrefix(address, "https")
	address += "?" + url.Values{"session_id": {session}, "colab-runtime-proxy-token": {p.Token}, "token": {p.Token}, "authuser": {"0"}}.Encode()
	config, e := websocket.NewConfig(address, p.URL)
	if e != nil {
		return nil, errors.New("无法建立 CPU 诊断通道")
	}
	config.Header.Set("X-Colab-Runtime-Proxy-Token", p.Token)
	config.Header.Set("Authorization", "Bearer "+p.Token)
	config.Header.Set("X-Colab-Client-Agent", "vscode")
	config.Dialer = &net.Dialer{Timeout: 10 * time.Second}
	return config, nil
}
