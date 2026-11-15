package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/egoist/mygo"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// Protocol reference: googlecolab/google-colab-cli, Client.list_assignments/unassign.
// Credentials stay in Go. No Python CLI process or cookie scraping is involved.
const tunnelBase = "https://colab.research.google.com/tun/m/"

var errPublicAPIDisabled = errors.New("Colab API HTTP 403 / SERVICE_DISABLED：凭据可刷新，但其所属 Google Cloud 项目未启用 Colab API。请在该项目启用 API；无权管理该项目时，改用自己的 OAuth client 授权")
var endpointPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,200}$`)

type assignment struct {
	Endpoint    string `json:"endpoint"`
	Variant     int    `json:"variant"`
	Accelerator string `json:"accelerator"`
	Shape       int    `json:"machineShape"`
	Proxy       struct {
		URL   string `json:"url"`
		Token string `json:"token"`
	} `json:"runtimeProxyInfo"`
}

func tunnelRequest(client *http.Client, base, access, method, path, xsrf string, out any) error {
	req, e := http.NewRequest(method, base+path, nil)
	if e != nil {
		return errors.New("无法构造兼容接口请求")
	}
	req.Header.Set("Authorization", "Bearer "+access)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Colab-Client-Agent", "colab-cli")
	if xsrf != "" {
		req.Header.Set("X-Goog-Colab-Token", xsrf)
	}
	resp, e := client.Do(req)
	if e != nil {
		return errors.New("无法连接 EasyColab 兼容接口，请检查网络")
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("EasyColab 兼容接口 HTTP %d，请稍后重试或检查账号资格", resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	b, e := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if e != nil {
		return errors.New("无法读取兼容接口响应")
	}
	b = []byte(strings.TrimPrefix(string(b), ")]}'"))
	if json.Unmarshal(b, out) != nil {
		return errors.New("兼容接口返回格式无效")
	}
	return nil
}
func decodeEndpoint(name string) (string, error) {
	if !strings.HasPrefix(name, "compat/") {
		return "", errors.New("无效兼容实例名称")
	}
	b, e := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(name, "compat/"))
	if e != nil || !endpointPattern.Match(b) {
		return "", errors.New("无效兼容实例名称")
	}
	return string(b), nil
}
func mapAssignment(a assignment) Runtime {
	r := Runtime{Name: "compat/" + base64.RawURLEncoding.EncodeToString([]byte(a.Endpoint)), DisplayName: a.Endpoint, Backend: "EasyColab 兼容接口"}
	r.RuntimeSpec = Spec{Variant: map[int]string{0: "VARIANT_CPU", 1: "VARIANT_GPU", 2: "VARIANT_TPU"}[a.Variant], Accelerator: a.Accelerator, Shape: map[int]string{0: "SHAPE_STANDARD", 1: "SHAPE_HIGHMEM"}[a.Shape]}
	r.ConnectionInfo.URL = a.Proxy.URL
	return r
}
func (s *Studio) compatibleList() ([]Runtime, error) {
	token, e := access()
	if e != nil {
		return nil, e
	}
	return fetchAssignments(httpClient, tunnelBase, token)
}
func fetchAssignments(client *http.Client, base, token string) ([]Runtime, error) {
	var e error
	var data struct {
		Assignments []assignment `json:"assignments"`
	}
	if e = tunnelRequest(client, base, token, "GET", "assignments?authuser=0", "", &data); e != nil {
		return nil, e
	}
	rows := make([]Runtime, 0, len(data.Assignments))
	for _, a := range data.Assignments {
		if !endpointPattern.MatchString(a.Endpoint) {
			return nil, errors.New("兼容接口返回无效 endpoint")
		}
		rows = append(rows, mapAssignment(a))
	}
	return rows, nil
}
func (s *Studio) compatibleOpen(name string) error {
	if _, e := decodeEndpoint(name); e != nil {
		return e
	}
	rows, e := s.compatibleList()
	if e != nil {
		return e
	}
	for _, r := range rows {
		if r.Name == name {
			return openRuntimeURL(r.ConnectionInfo.URL)
		}
	}
	return errors.New("实例已不存在，请刷新")
}
func (s *Studio) compatibleRelease(name string) error {
	endpoint, e := decodeEndpoint(name)
	if e != nil {
		return e
	}
	token, e := access()
	if e != nil {
		return e
	}
	return releaseAssignment(httpClient, tunnelBase, token, endpoint)
}
func releaseAssignment(client *http.Client, base, token, endpoint string) error {
	if !endpointPattern.MatchString(endpoint) {
		return errors.New("无效实例 endpoint")
	}
	path := "unassign/" + url.PathEscape(endpoint) + "?authuser=0"
	var challenge struct {
		Token string `json:"token"`
	}
	if e := tunnelRequest(client, base, token, "GET", path, "", &challenge); e != nil {
		return e
	}
	if challenge.Token == "" {
		return errors.New("释放确认令牌缺失，未提交释放请求")
	}
	return tunnelRequest(client, base, token, "POST", path, challenge.Token, nil)
}
func (s *Studio) Backend() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return map[string]any{"compatible": s.compatible}
}

func fetchRuntimes(client *http.Client, base, tunnel, token string) ([]Runtime, bool, error) {
	var data struct {
		Runtimes []Runtime `json:"runtimes"`
	}
	e := request(context.Background(), client, base, token, "GET", "runtimes", nil, &data)
	if errors.Is(e, errPublicAPIDisabled) {
		rows, ce := fetchAssignments(client, tunnel, token)
		return rows, ce == nil, ce
	}
	if data.Runtimes == nil {
		data.Runtimes = []Runtime{}
	}
	return data.Runtimes, false, e
}
func (s *Studio) OpenColab() error {
	return mygo.Shell.OpenExternal("https://colab.research.google.com/")
}
