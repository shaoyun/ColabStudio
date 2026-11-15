package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const logPath = "/content/easycolab_server.log"
const maxLogBytes = 64 << 10

var errLogMissing = errors.New("log missing")

type InstanceLog struct {
	Path      string `json:"path"`
	Text      string `json:"text"`
	Lines     int    `json:"lines"`
	Truncated bool   `json:"truncated"`
	Missing   bool   `json:"missing"`
	Message   string `json:"message"`
	Updated   string `json:"updated"`
	Bytes     int64  `json:"bytes"`
}

func logRequest(client *http.Client, p runtimeProxy, path string, query url.Values, rangeHeader string) (*http.Response, error) {
	address, e := proxyURL(p, path)
	if e != nil {
		return nil, e
	}
	u, e := url.Parse(address)
	if e != nil {
		return nil, errors.New("日志路径无效")
	}
	u.RawQuery = query.Encode()
	req, e := http.NewRequest("GET", u.String(), nil)
	if e != nil {
		return nil, errors.New("无法构造日志读取请求")
	}
	req.Header.Set("X-Colab-Runtime-Proxy-Token", p.Token)
	if rangeHeader != "" {
		req.Header.Set("Range", rangeHeader)
	}
	safe := *client
	safe.Timeout = 12 * time.Second
	safe.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, e := safe.Do(req)
	if e != nil {
		return nil, errors.New("日志暂时无法连接，请稍后刷新")
	}
	if resp.StatusCode == 404 {
		resp.Body.Close()
		return nil, errLogMissing
	}
	if resp.StatusCode != 200 && resp.StatusCode != 206 {
		resp.Body.Close()
		return nil, fmt.Errorf("读取日志 HTTP %d，实例可能已断开或没有文件访问权限", resp.StatusCode)
	}
	return resp, nil
}
func readInstanceLog(client *http.Client, p runtimeProxy, lines int, secrets ...string) (InstanceLog, error) {
	result := InstanceLog{Path: logPath, Updated: time.Now().UTC().Format(time.RFC3339)}
	if lines < 1 || lines > 2000 {
		return result, errors.New("日志行数需介于 1 与 2000 之间")
	}
	var metadata struct {
		Size *int64 `json:"size"`
		Type string `json:"type"`
	}
	var file string
	for _, candidate := range []string{"content/easycolab_server.log", "easycolab_server.log"} {
		resp, e := logRequest(client, p, "api/contents/"+candidate, url.Values{"content": {"0"}}, "")
		if errors.Is(e, errLogMissing) {
			continue
		}
		if e != nil {
			return result, e
		}
		b, e := io.ReadAll(io.LimitReader(resp.Body, 128<<10))
		resp.Body.Close()
		if e != nil {
			return result, errors.New("日志文件信息读取失败")
		}
		b = []byte(strings.TrimPrefix(strings.TrimSpace(string(b)), ")]}'"))
		if json.Unmarshal(b, &metadata) != nil || metadata.Type != "file" {
			return result, errors.New("目标不是可读取的日志文件")
		}
		file = candidate
		break
	}
	if file == "" {
		result.Missing = true
		result.Message = "尚未找到 /content/easycolab_server.log。该实例可能没有运行 EasyColab 服务，或日志已被清理。"
		return result, nil
	}
	if metadata.Size != nil {
		result.Bytes = *metadata.Size
		if *metadata.Size == 0 {
			result.Message = "日志文件为空，等待服务输出。"
			return result, nil
		}
	}
	resp, e := logRequest(client, p, "files/"+file, nil, fmt.Sprintf("bytes=-%d", maxLogBytes))
	if errors.Is(e, errLogMissing) {
		result.Missing = true
		result.Message = "日志文件暂不存在，可能正在轮转，请稍后刷新。"
		return result, nil
	}
	if e != nil {
		return result, e
	}
	defer resp.Body.Close()
	if strings.Contains(resp.Header.Get("Content-Type"), "text/html") {
		return result, errors.New("实例没有返回日志文本，请重新连接后刷新")
	}
	b, e := io.ReadAll(io.LimitReader(resp.Body, maxLogBytes+1))
	if e != nil {
		return result, errors.New("日志读取中断，请刷新重试")
	}
	if len(b) > maxLogBytes {
		return result, errors.New("日志较大且服务器未支持尾部读取，已停止下载；请在 Colab 查看完整日志")
	}
	result.Truncated = resp.StatusCode == 206 && (metadata.Size == nil || *metadata.Size > int64(len(b)))
	if resp.StatusCode == 200 && metadata.Size != nil && *metadata.Size > int64(len(b)) {
		return result, errors.New("日志读取不完整，请刷新重试")
	}
	text := strings.ToValidUTF8(string(b), "�")
	if result.Truncated {
		if i := strings.IndexByte(text, '\n'); i >= 0 {
			text = text[i+1:]
		} else {
			text = ""
			result.Message = "日志最后一行超过显示上限，请在 Colab 查看。"
		}
	}
	text = redactLog(text, append(secrets, p.Token)...)
	result.Text, result.Lines, result.Truncated = tailLog(text, lines, result.Truncated)
	if result.Text == "" && result.Message == "" {
		result.Message = "日志文件为空，等待服务输出。"
	}
	return result, nil
}
func tailLog(text string, lines int, truncated bool) (string, int, bool) {
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	text = strings.TrimRight(text, "\n")
	if text == "" {
		return "", 0, truncated
	}
	parts := strings.Split(text, "\n")
	if len(parts) > lines {
		parts = parts[len(parts)-lines:]
		truncated = true
	}
	return strings.Join(parts, "\n"), len(parts), truncated
}

var logANSI = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)
var logToyboxToken = regexp.MustCompile(`\bsk-toybox-[A-Za-z0-9_-]+\b`)
var logAuthorization = regexp.MustCompile(`(?i)\b(?:Bearer|Basic)\s+[A-Za-z0-9._~+/=-]+`)
var logQuerySecret = regexp.MustCompile(`(?i)([?&](?:api_key|token|key)=)[^&\s"'#]+`)
var logAssignments = regexp.MustCompile(`(?i)((?:\b(?:access[_-]?token|refresh[_-]?token|id[_-]?token|client[_-]?secret|api[_ -]?key|x-api-key|x-goog-api-key|authorization|admin[_-]?password|password|token|colab-runtime-proxy-token)\b|密码|密钥|令牌)["']?\s*[:=：]\s*["']?)[^\s,"'&}]+`)

func redactLog(text string, secrets ...string) string {
	text = logANSI.ReplaceAllString(text, "")
	text = logToyboxToken.ReplaceAllString(text, "[凭据已隐藏]")
	text = logAuthorization.ReplaceAllString(text, "[凭据已隐藏]")
	text = logQuerySecret.ReplaceAllString(text, "${1}[已隐藏]")
	text = logAssignments.ReplaceAllString(text, "${1}[已隐藏]")
	for _, secret := range secrets {
		if len(secret) >= 8 {
			text = strings.ReplaceAll(text, secret, "[凭据已隐藏]")
		}
	}
	return text
}
func (s *Studio) Logs(name string, lines int) (InstanceLog, error) {
	if lines < 1 || lines > 2000 {
		return InstanceLog{}, errors.New("日志行数需介于 1 与 2000 之间")
	}
	s.mu.Lock()
	p, e := s.proxy(name)
	var secrets []string
	if e == nil {
		if b, re := store.Get("access-token"); re == nil {
			var token Token
			if json.Unmarshal(b, &token) == nil {
				secrets = append(secrets, token.Access, token.Refresh)
			}
		}
		if b, re := store.Get("refresh-token"); re == nil {
			secrets = append(secrets, string(b))
		}
	}
	s.mu.Unlock()
	if e != nil {
		return InstanceLog{}, e
	}
	return readInstanceLog(httpClient, p, lines, secrets...)
}
