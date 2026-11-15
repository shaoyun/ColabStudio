package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const maxRPCBodyBytes = 1 << 20

type rpcHandler func(s *Studio, args []json.RawMessage) (any, error)

type rpcRequestError struct {
	msg string
}

func (e rpcRequestError) Error() string { return e.msg }

type rpcNotFoundError struct {
	method string
}

func (e rpcNotFoundError) Error() string { return "未知 RPC 方法: " + e.method }

func rpcBadRequest(format string, args ...any) error {
	return rpcRequestError{msg: fmt.Sprintf(format, args...)}
}

func unmarshalRPCArg(raw json.RawMessage, dst any) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return errors.New("参数不能为空")
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	if err := dec.Decode(dst); err != nil {
		return err
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return errors.New("存在多余的 JSON 内容")
	}
	return nil
}

func decodeRPCArgs(raw []json.RawMessage, dst ...any) error {
	if len(raw) != len(dst) {
		return rpcBadRequest("参数数量不匹配：需要 %d 个，收到 %d 个", len(dst), len(raw))
	}
	for i, target := range dst {
		if err := unmarshalRPCArg(raw[i], target); err != nil {
			return rpcBadRequest("第 %d 个参数无效: %v", i+1, err)
		}
	}
	return nil
}

func rpcNoArgVal[R any](fn func(*Studio) R) rpcHandler {
	return func(s *Studio, args []json.RawMessage) (any, error) {
		if err := decodeRPCArgs(args); err != nil {
			return nil, err
		}
		return fn(s), nil
	}
}

func rpcNoArg[R any](fn func(*Studio) (R, error)) rpcHandler {
	return func(s *Studio, args []json.RawMessage) (any, error) {
		if err := decodeRPCArgs(args); err != nil {
			return nil, err
		}
		return fn(s)
	}
}

func rpcNoArgErr(fn func(*Studio) error) rpcHandler {
	return func(s *Studio, args []json.RawMessage) (any, error) {
		if err := decodeRPCArgs(args); err != nil {
			return nil, err
		}
		return nil, fn(s)
	}
}

func rpcArg1[A, R any](fn func(*Studio, A) (R, error)) rpcHandler {
	return func(s *Studio, args []json.RawMessage) (any, error) {
		var a A
		if err := decodeRPCArgs(args, &a); err != nil {
			return nil, err
		}
		return fn(s, a)
	}
}

func rpcArg1Err[A any](fn func(*Studio, A) error) rpcHandler {
	return func(s *Studio, args []json.RawMessage) (any, error) {
		var a A
		if err := decodeRPCArgs(args, &a); err != nil {
			return nil, err
		}
		return nil, fn(s, a)
	}
}

func rpcArg2[A, B, R any](fn func(*Studio, A, B) (R, error)) rpcHandler {
	return func(s *Studio, args []json.RawMessage) (any, error) {
		var a A
		var b B
		if err := decodeRPCArgs(args, &a, &b); err != nil {
			return nil, err
		}
		return fn(s, a, b)
	}
}

func rpcArg2Err[A, B any](fn func(*Studio, A, B) error) rpcHandler {
	return func(s *Studio, args []json.RawMessage) (any, error) {
		var a A
		var b B
		if err := decodeRPCArgs(args, &a, &b); err != nil {
			return nil, err
		}
		return nil, fn(s, a, b)
	}
}

var studioRPCMethods = map[string]rpcHandler{
	// Account & general status
	"Backend":       rpcNoArgVal((*Studio).Backend),
	"DriveStatus":   rpcNoArgVal((*Studio).DriveStatus),
	"ServiceStatus": rpcNoArgVal((*Studio).ServiceStatus),
	"Status":        rpcNoArgVal((*Studio).Status),

	// 0-arg returning (value, error)
	"ClearTunnelConfig":             rpcNoArg((*Studio).ClearTunnelConfig),
	"DeploymentProgress":            rpcNoArg((*Studio).DeploymentProgress),
	"Profile":                       rpcNoArg((*Studio).Profile),
	"Runtimes":                      rpcNoArg((*Studio).Runtimes),
	"Specs":                         rpcNoArg((*Studio).Specs),
	"ToyboxAPIToken":                rpcNoArg((*Studio).ToyboxAPIToken),
	"ToyboxAccessStatus":            rpcNoArg((*Studio).ToyboxAccessStatus),
	"ToyboxPassword":                rpcNoArg((*Studio).ToyboxPassword),
	"TunnelConfigStatus":            rpcNoArg((*Studio).TunnelConfigStatus),
	"Usage":                         rpcNoArg((*Studio).Usage),
	"BeginWebLogin":                 rpcNoArg((*Studio).beginWebLogin),
	"ToyboxVerifiedURL":             rpcNoArg((*Studio).toyboxVerifiedURL),
	"ToyboxAPIBaseURL":              rpcNoArg((*Studio).toyboxAPIBaseURL),
	"OpenToyboxURL":                 rpcNoArg((*Studio).openToyboxURL),
	"EnsureDriveFolderURL":          rpcNoArg((*Studio).ensureDriveFolderURL),
	"CompatibleURL":                 rpcNoArg(func(*Studio) (string, error) { return "https://colab.research.google.com/", nil }),
	"TryImportLocalEasyColabConfig": rpcNoArg((*Studio).tryImportLocalEasyColabConfig),

	// 0-arg returning error
	"BeginRemoteLogin":      rpcNoArgErr((*Studio).BeginRemoteLogin),
	"CopyToyboxAPIBaseURL":  rpcNoArgErr((*Studio).CopyToyboxAPIBaseURL),
	"CopyToyboxAPIToken":    rpcNoArgErr((*Studio).CopyToyboxAPIToken),
	"CopyToyboxPassword":    rpcNoArgErr((*Studio).CopyToyboxPassword),
	"CopyToyboxURL":         rpcNoArgErr((*Studio).CopyToyboxURL),
	"Disconnect":            rpcNoArgErr((*Studio).Disconnect),
	"ImportDriveClient":     rpcNoArgErr((*Studio).ImportDriveClient),
	"ImportEasyColabConfig": rpcNoArgErr((*Studio).ImportEasyColabConfig),
	"Login":                 rpcNoArgErr((*Studio).Login),
	"OpenColab":             rpcNoArgErr((*Studio).OpenColab),
	"OpenDriveFolder":       rpcNoArgErr((*Studio).OpenDriveFolder),
	"OpenToybox":            rpcNoArgErr((*Studio).OpenToybox),
	"RefreshService":        rpcNoArgErr((*Studio).RefreshService),
	"ReuseExistingLogin":    rpcNoArgErr((*Studio).ReuseExistingLogin),
	"UseEasyColabDrive":     rpcNoArgErr((*Studio).UseEasyColabDrive),

	// 1-arg returning (value, error)
	"BeginDriveLogin":    rpcArg1((*Studio).BeginDriveLogin),
	"BeginWebDriveLogin": rpcArg1((*Studio).beginWebDriveLogin),
	"CPU":                rpcArg1((*Studio).CPU),
	"Create":             rpcArg1((*Studio).Create),
	"Operation":          rpcArg1((*Studio).Operation),
	"ResourceHistory":    rpcArg1((*Studio).ResourceHistory),
	"Resources":          rpcArg1((*Studio).Resources),
	"RuntimeOpenURL":     rpcArg1((*Studio).runtimeOpenURL),
	"ToyboxLogs":         rpcArg1((*Studio).ToyboxLogs),

	// 1-arg returning error
	"ApplyToyboxAccess":            rpcArg1Err((*Studio).ApplyToyboxAccess),
	"ApplyToyboxLocalization":      rpcArg1Err((*Studio).ApplyToyboxLocalization),
	"CancelDriveLogin":             rpcArg1Err((*Studio).CancelDriveLogin),
	"Open":                         rpcArg1Err((*Studio).Open),
	"RetryToybox":                  rpcArg1Err((*Studio).RetryToybox),
	"RotateToyboxAPIToken":         rpcArg1Err((*Studio).RotateToyboxAPIToken),
	"StartToybox":                  rpcArg1Err((*Studio).StartToybox),
	"StopToybox":                   rpcArg1Err((*Studio).StopToybox),
	"SubmitRemoteCode":             rpcArg1Err((*Studio).SubmitRemoteCode),
	"WakeToybox":                   rpcArg1Err((*Studio).WakeToybox),
	"ImportEasyColabConfigJSON":    rpcArg1Err(func(s *Studio, raw string) error { return s.importEasyColabConfigBytes([]byte(raw)) }),
	"ImportDriveClientJSON":        rpcArg1Err(func(s *Studio, raw string) error { return s.importDriveClientBytes([]byte(raw)) }),
	"ImportExistingCredentialJSON": rpcArg1Err(func(s *Studio, raw string) error { return s.importExistingCredentialBytes([]byte(raw)) }),
	"ImportRcloneDriveConf":        rpcArg1Err(func(s *Studio, raw string) error { return s.importRcloneDriveBytes([]byte(raw)) }),

	// 2-arg methods
	"Logs":             rpcArg2((*Studio).Logs),
	"SaveTunnelConfig": rpcArg2((*Studio).SaveTunnelConfig),
	"Release":          rpcArg2Err((*Studio).Release),
	"SaveConfig":       rpcArg2Err((*Studio).SaveConfig),
	"SaveMachineType":  rpcArg2Err((*Studio).SaveMachineType),
	"SaveServiceRule":  rpcArg2Err((*Studio).SaveServiceRule),
	"SubmitDriveCode":  rpcArg2Err((*Studio).SubmitDriveCode),
}

func dispatchStudioRPC(s *Studio, method string, rawArgs []json.RawMessage) (any, error) {
	if s == nil {
		return nil, errors.New("服务实例尚未初始化")
	}
	name := strings.TrimPrefix(strings.TrimSpace(method), "Studio.")
	if name == "" {
		return nil, rpcNotFoundError{method: method}
	}
	h, ok := studioRPCMethods[name]
	if !ok {
		return nil, rpcNotFoundError{method: name}
	}
	return h(s, rawArgs)
}

func readRPCBody(w http.ResponseWriter, r *http.Request) ([]json.RawMessage, error) {
	if r.Body == nil {
		return nil, nil
	}
	defer r.Body.Close()
	r.Body = http.MaxBytesReader(w, r.Body, maxRPCBodyBytes)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return nil, rpcBadRequest("请求体过大")
		}
		return nil, rpcBadRequest("读取请求体失败")
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, nil
	}
	if trimmed[0] == '[' {
		var args []json.RawMessage
		if err := unmarshalRPCArg(trimmed, &args); err != nil {
			return nil, rpcBadRequest("参数数组格式无效")
		}
		return args, nil
	}
	var env struct {
		Args []json.RawMessage `json:"args"`
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&env); err != nil {
		return nil, rpcBadRequest("请求 JSON 格式无效")
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return nil, rpcBadRequest("请求存在多余的 JSON 内容")
	}
	return env.Args, nil
}

func (srv *webServer) handleRPC(w http.ResponseWriter, r *http.Request) {
	method := r.PathValue("method")
	if method == "" {
		method = strings.TrimPrefix(r.URL.Path, "/api/rpc/")
	}
	rawArgs, err := readRPCBody(w, r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	res, err := dispatchStudioRPC(srv.studio, method, rawArgs)
	if err != nil {
		var notFound rpcNotFoundError
		var badReq rpcRequestError
		switch {
		case errors.As(err, &notFound):
			writeJSONError(w, http.StatusNotFound, err.Error())
		case errors.As(err, &badReq):
			writeJSONError(w, http.StatusBadRequest, err.Error())
		default:
			writeJSONError(w, http.StatusUnprocessableEntity, err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"result": res,
	})
}
