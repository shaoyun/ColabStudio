package main

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
)

var (
	dataDirMu     sync.RWMutex
	customDataDir string
	serverMode    atomic.Bool
)

func setWebServerMode(enabled bool) {
	serverMode.Store(enabled)
}

func isWebServerMode() bool {
	return serverMode.Load()
}

func setDataDir(dir string) error {
	trimmed := strings.TrimSpace(dir)
	dataDirMu.Lock()
	defer dataDirMu.Unlock()
	if trimmed == "" {
		customDataDir = ""
		return nil
	}
	abs, err := filepath.Abs(trimmed)
	if err != nil {
		return err
	}
	customDataDir = abs
	return nil
}

func dataDir() string {
	dataDirMu.RLock()
	custom := customDataDir
	dataDirMu.RUnlock()
	if custom != "" {
		return custom
	}
	if env := strings.TrimSpace(os.Getenv("COLABSTUDIO_DATA_DIR")); env != "" {
		if abs, err := filepath.Abs(env); err == nil {
			return abs
		}
		return env
	}
	if d, err := os.UserConfigDir(); err == nil && d != "" {
		return filepath.Join(d, "ColabStudio")
	}
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		return filepath.Join(h, ".config", "ColabStudio")
	}
	return filepath.Join(os.TempDir(), "ColabStudio")
}

func configPath() string {
	return filepath.Join(dataDir(), "config.json")
}
