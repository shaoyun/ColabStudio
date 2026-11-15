package main

import (
	"errors"
	"io"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type TransferProgress struct {
	Phase          string   `json:"phase"`
	CurrentFile    string   `json:"currentFile"`
	FileIndex      int      `json:"fileIndex"`
	TotalFiles     int      `json:"totalFiles"`
	VerifiedFiles  int      `json:"verifiedFiles"`
	CurrentBytes   int64    `json:"currentBytes"`
	FileBytes      int64    `json:"fileBytes"`
	CompletedBytes int64    `json:"completedBytes"`
	TotalBytes     int64    `json:"totalBytes"`
	SpeedBytes     float64  `json:"speedBytes"`
	ETA            *float64 `json:"etaSeconds"`
	StartedAt      float64  `json:"startedAt"`
	UpdatedAt      float64  `json:"updatedAt"`
}
type ModelLoadStatus struct {
	Phase     string   `json:"phase"`
	Requested string   `json:"requested"`
	LoadedMiB *float64 `json:"loadedMiB,omitempty"`
}
type RemoteReadiness struct {
	Stage     string  `json:"stage"`
	Health    bool    `json:"health"`
	Fresh     bool    `json:"fresh"`
	Heartbeat float64 `json:"heartbeat"`
	URL       string  `json:"url"`
	Version   string  `json:"version,omitempty"`
	Outdated  bool    `json:"outdated,omitempty"`
}
type ServiceProgress struct {
	Remote          *RemoteReadiness  `json:"remote,omitempty"`
	WeightsReady    bool              `json:"weightsReady"`
	WeightsEvidence string            `json:"weightsEvidence,omitempty"`
	Transfer        *TransferProgress `json:"transfer"`
	Model           ModelLoadStatus   `json:"model"`
	Legacy          bool              `json:"legacy"`
	Message         string            `json:"message"`
	Updated         time.Time         `json:"updated"`
}

var modelRequested = regexp.MustCompile(`Requested to load ([A-Za-z0-9_.-]{1,100})\b`)
var modelLoadedMB = regexp.MustCompile(`([0-9]+(?:\.[0-9]+)?) MB loaded`)
var legacyModel = regexp.MustCompile(`MODEL\s+(\d+)\s+(\d+)\s+([^\r\n]+)`)

func parseModelLoad(text string) ModelLoadStatus {
	out := ModelLoadStatus{Phase: "on_demand"}
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r", "\n"), "\n") {
		if m := modelRequested.FindStringSubmatch(line); m != nil {
			out.Phase = "loading"
			out.Requested = m[1]
			out.LoadedMiB = nil
		}
		if strings.Contains(line, "loaded completely") || strings.Contains(line, "loaded partially") {
			out.Phase = "loaded"
			if strings.Contains(line, "loaded partially") {
				out.Phase = "partial"
			}
			if m := modelLoadedMB.FindStringSubmatch(line); m != nil {
				if v, e := strconv.ParseFloat(m[1], 64); e == nil {
					out.LoadedMiB = &v
				}
			}
		}
		if strings.Contains(line, "OutOfMemoryError") || strings.Contains(line, "CUDA out of memory") {
			out.Phase = "error"
		}
	}
	return out
}
func validateTransfer(p *TransferProgress) error {
	if p.CurrentBytes < 0 || p.FileBytes < 0 || p.CompletedBytes < 0 || p.TotalBytes < 0 || p.FileIndex < 0 || p.TotalFiles < 0 || p.VerifiedFiles < 0 || p.TotalFiles > 1000 || p.FileIndex > p.TotalFiles || p.VerifiedFiles > p.TotalFiles || p.CurrentBytes > p.FileBytes || p.CompletedBytes > p.TotalBytes || math.IsNaN(p.SpeedBytes) || math.IsInf(p.SpeedBytes, 0) || p.SpeedBytes < 0 {
		return errors.New("远端进度计数无效")
	}
	if len(p.CurrentFile) > 300 || strings.ContainsAny(p.CurrentFile, "/\\\r\n") {
		return errors.New("远端模型文件名无效")
	}
	if p.ETA != nil && (*p.ETA < 0 || math.IsNaN(*p.ETA) || math.IsInf(*p.ETA, 0)) {
		p.ETA = nil
	}
	return nil
}
func readProgressLog(p runtimeProxy, root, kind string) (string, error) {
	resp, e := logRequest(httpClient, p, "files/"+root+"/"+kind+".log", nil, "bytes=-65536")
	if e != nil {
		return "", e
	}
	defer resp.Body.Close()
	b, e := io.ReadAll(io.LimitReader(resp.Body, 65537))
	if e != nil || len(b) > 65536 {
		return "", errors.New("日志范围读取未确认")
	}
	return redactLog(string(b), p.Token), nil
}
func (s *Studio) DeploymentProgress() (ServiceProgress, error) {
	r := s.services().get()
	result := ServiceProgress{Updated: time.Now(), Model: ModelLoadStatus{Phase: "on_demand"}}
	if r.Runtime == "" || r.RemoteRoot == "" {
		result.Message = "部署后显示下载与模型加载状态"
		return result, nil
	}
	if _, e := s.services().identity(r); e != nil {
		return result, e
	}
	p, e := s.proxy(r.Runtime)
	if e != nil {
		return result, e
	}
	var owner remoteState
	if e = remoteJSON(p, r.RemoteRoot, "state.json", &owner); e != nil {
		return result, e
	}
	if owner.Owner != r.ID || owner.Binding != r.Binding {
		return result, errors.New("远端服务身份不匹配，未读取进度")
	}
	if !validRemoteVersion(owner.Version) {
		return result, errors.New("远端服务版本标记无效，未读取进度")
	}
	result.Remote = readinessFromState(owner, time.Now())
	var progress TransferProgress
	if e = remoteJSON(p, r.RemoteRoot, "progress.json", &progress); e == nil {
		if e = validateTransfer(&progress); e != nil {
			return result, e
		}
		result.Transfer = &progress
		result.WeightsReady = weightsVerified(progress)
		if result.WeightsReady {
			result.WeightsEvidence = "sha256"
		}
	} else if strings.Contains(e.Error(), "HTTP 404") {
		result.Legacy = true
		result.Message = "当前部署未提供字节进度，正在显示实时日志；不会重启机器"
		if log, le := readProgressLog(p, r.RemoteRoot, "deployment"); le == nil {
			matches := legacyModel.FindAllStringSubmatch(log, -1)
			if len(matches) > 0 {
				last := matches[len(matches)-1]
				i, _ := strconv.Atoi(last[1])
				total, _ := strconv.Atoi(last[2])
				legacy := TransferProgress{Phase: "legacy_download", FileIndex: i, TotalFiles: total, CurrentFile: strings.TrimSpace(last[3])}
				if validateTransfer(&legacy) == nil {
					result.Transfer = &legacy
				}
			}
		}
	} else {
		return result, e
	}
	if !result.WeightsReady && (owner.Stage == "online" || owner.Stage == "starting") {
		var installed struct {
			Version string `json:"version"`
		}
		if remoteJSON(p, r.RemoteRoot, "installed.json", &installed) == nil && validRemoteVersion(installed.Version) && installed.Version == owner.Version {
			result.WeightsReady = true
			result.WeightsEvidence = "installation"
		}
	}
	if text, e := readProgressLog(p, r.RemoteRoot, "comfyui"); e == nil {
		result.Model = parseModelLoad(text)
	} else if e != errLogMissing {
		result.Model.Phase = "unknown"
	}
	if owner.Stage == "failed" {
		result.Message = "部署失败；下方保留最后进度，请查看部署日志"
	} else if time.Since(time.Unix(int64(owner.Heartbeat), 0)) > 90*time.Second {
		result.Message = "远端心跳已过期，进度可能不是最新状态"
	} else if result.Message == "" && owner.Version != toyboxVersion {
		result.Message = "远端当前运行旧版本 " + shortVersion(owner.Version) + "，进度读取与备份停止均正常可用；任务空闲时可热升级至 " + shortVersion(toyboxVersion)
	}
	return result, nil
}

func weightsVerified(p TransferProgress) bool {
	return validateTransfer(&p) == nil && p.TotalFiles > 0 && p.VerifiedFiles == p.TotalFiles && p.TotalBytes > 0 && p.CompletedBytes == p.TotalBytes
}
func readinessFromState(r remoteState, now time.Time) *RemoteReadiness {
	age := float64(now.Unix()) - r.Heartbeat
	out := &RemoteReadiness{
		Stage:     r.Stage,
		Health:    r.Health,
		Fresh:     r.Heartbeat > 0 && age >= -10 && age <= 90,
		Heartbeat: r.Heartbeat,
		Version:   r.Version,
		Outdated:  validRemoteVersion(r.Version) && r.Version != toyboxVersion,
	}
	if address, e := urlForToybox(r.URL); e == nil {
		out.URL = address
	}
	return out
}
