package main

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

type presetKeyStore interface {
	PresetKey() ([]byte, error)
}

type fileKeyStore struct {
	dir string
}

func newFileKeyStore(dir string) *fileKeyStore {
	return &fileKeyStore{dir: dir}
}

func parseVaultKeyString(raw string) ([]byte, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil, errSecretNotFound
	}
	if len(s) == 64 {
		if b, err := hex.DecodeString(s); err == nil && len(b) == 32 {
			return b, nil
		}
	}
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding,
		base64.RawStdEncoding,
		base64.URLEncoding,
		base64.RawURLEncoding,
	} {
		if b, err := enc.DecodeString(s); err == nil && len(b) == 32 {
			return b, nil
		}
	}
	return nil, errors.New("COLABSTUDIO_VAULT_KEY 无效：须为编码后 32 字节密钥（64 位十六进制或 Base64）")
}

func (s *fileKeyStore) PresetKey() ([]byte, error) {
	raw := strings.TrimSpace(os.Getenv("COLABSTUDIO_VAULT_KEY"))
	if raw == "" {
		return nil, errSecretNotFound
	}
	return parseVaultKeyString(raw)
}

func (s *fileKeyStore) keyFilePath() string {
	return filepath.Join(s.dir, "vault.key")
}

func (s *fileKeyStore) Get(string) ([]byte, error) {
	if preset, err := s.PresetKey(); !errors.Is(err, errSecretNotFound) {
		return preset, err
	}
	b, err := os.ReadFile(s.keyFilePath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, errSecretNotFound
		}
		return nil, err
	}
	if len(b) == 32 {
		return b, nil
	}
	// Tolerate a text-encoded key if an operator placed one manually.
	if parsed, perr := parseVaultKeyString(string(b)); perr == nil {
		return parsed, nil
	}
	return nil, errors.New("本机主密钥文件损坏（vault.key 长度无效）")
}

func (s *fileKeyStore) Set(_ string, key []byte) error {
	if len(key) != 32 {
		return errors.New("主密钥长度必须为 32 字节")
	}
	if preset, err := s.PresetKey(); err == nil {
		if !bytes.Equal(preset, key) {
			return errors.New("外部设置的 COLABSTUDIO_VAULT_KEY 与期望密钥不一致")
		}
		return nil
	} else if !errors.Is(err, errSecretNotFound) {
		return err
	}
	if err := os.MkdirAll(s.dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(s.dir, ".vault-key-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(key)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return errors.New("无法写入本机主密钥文件 vault.key")
	}
	if err = os.Rename(f.Name(), s.keyFilePath()); err != nil {
		return err
	}
	if dir, err := os.Open(s.dir); err == nil {
		_ = dir.Sync()
		dir.Close()
	}
	return nil
}
