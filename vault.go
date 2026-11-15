package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
)

type keyStore interface {
	Get(string) ([]byte, error)
	Set(string, []byte) error
}

// credentialVault stores one encryption key in Keychain. All credentials are
// encrypted on disk and cached in memory; API refresh never reopens Keychain.
// A denied read is latched for this process so retries cannot cause prompt loops.
type credentialVault struct {
	mu      sync.Mutex
	path    string
	backend keyStore
	loaded  bool
	loadErr error
	key     []byte
	values  map[string][]byte
}

const vaultKeyName = "credential-vault-key-v1"

var vaultAAD = []byte("dev.colabstudio.desktop/credentials/v1")

func newCredentialVault(path string, backend keyStore) *credentialVault {
	return &credentialVault{path: path, backend: backend}
}
func vaultPath() string { return filepath.Join(filepath.Dir(configPath()), "credentials.enc") }
func (v *credentialVault) load() error {
	if v.loaded {
		return v.loadErr
	}
	v.loaded = true
	v.values = make(map[string][]byte)
	b, e := os.ReadFile(v.path)
	if os.IsNotExist(e) {
		return nil
	}
	if e != nil {
		v.loadErr = errors.New("无法读取本机加密登录文件")
		return v.loadErr
	}
	v.key, e = v.backend.Get(vaultKeyName)
	if e != nil {
		v.loadErr = errors.New("钥匙串访问未获允许，本次不再重复询问；重新打开应用后可重试")
		return v.loadErr
	}
	block, e := aes.NewCipher(v.key)
	if e != nil || len(v.key) != 32 {
		v.loadErr = errors.New("本机登录密钥无效，请重新连接账号")
		return v.loadErr
	}
	gcm, _ := cipher.NewGCM(block)
	if len(b) < gcm.NonceSize() {
		v.loadErr = errors.New("本机加密登录文件不完整")
		return v.loadErr
	}
	plain, e := gcm.Open(nil, b[:gcm.NonceSize()], b[gcm.NonceSize():], vaultAAD)
	if e != nil || json.Unmarshal(plain, &v.values) != nil {
		v.loadErr = errors.New("本机登录文件校验失败，未读取任何凭据")
		return v.loadErr
	}
	if v.values == nil {
		v.values = make(map[string][]byte)
	}
	return nil
}
func (v *credentialVault) Get(name string) ([]byte, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if e := v.load(); e != nil {
		return nil, e
	}
	value, ok := v.values[name]
	if !ok {
		return nil, errSecretNotFound
	}
	return append([]byte(nil), value...), nil
}
func (v *credentialVault) save(values map[string][]byte) error {
	if len(v.key) == 0 {
		var key []byte
		if preset, ok := v.backend.(presetKeyStore); ok {
			pk, pe := preset.PresetKey()
			if pe != nil && !errors.Is(pe, errSecretNotFound) {
				v.loadErr = pe
				return v.loadErr
			}
			if len(pk) == 32 {
				key = pk
			}
		}
		if len(key) == 0 {
			key = make([]byte, 32)
			if _, e := rand.Read(key); e != nil {
				return errors.New("无法生成本机登录密钥")
			}
			if e := v.backend.Set(vaultKeyName, key); e != nil {
				v.loadErr = errors.New("无法保存登录密钥，本次不再重复请求钥匙串；重新打开应用后可重试")
				return v.loadErr
			}
		}
		v.key = key
	}
	block, e := aes.NewCipher(v.key)
	if e != nil {
		return errors.New("本机登录密钥无效")
	}
	gcm, e := cipher.NewGCM(block)
	if e != nil {
		return e
	}
	plain, e := json.Marshal(values)
	if e != nil {
		return errors.New("无法编码登录凭据")
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, e = io.ReadFull(rand.Reader, nonce); e != nil {
		return errors.New("无法生成加密随机数")
	}
	encrypted := gcm.Seal(nonce, nonce, plain, vaultAAD)
	if e = os.MkdirAll(filepath.Dir(v.path), 0700); e != nil {
		return errors.New("无法创建加密登录目录")
	}
	f, e := os.CreateTemp(filepath.Dir(v.path), ".credentials-*")
	if e != nil {
		return errors.New("无法保存加密登录文件")
	}
	defer os.Remove(f.Name())
	if e = f.Chmod(0600); e == nil {
		_, e = f.Write(encrypted)
	}
	closeErr := f.Close()
	if e != nil || closeErr != nil {
		return errors.New("无法写入加密登录文件")
	}
	if e = os.Rename(f.Name(), v.path); e != nil {
		return errors.New("无法替换加密登录文件")
	}
	v.values = values
	return nil
}
func (v *credentialVault) Set(name string, value []byte) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if e := v.load(); e != nil {
		return e
	}
	next := make(map[string][]byte, len(v.values)+1)
	for k, b := range v.values {
		next[k] = b
	}
	next[name] = append([]byte(nil), value...)
	return v.save(next)
}
func (v *credentialVault) Delete(name string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if e := v.load(); e != nil {
		return e
	}
	if _, ok := v.values[name]; !ok {
		return nil
	}
	next := make(map[string][]byte, len(v.values))
	for k, b := range v.values {
		if k != name {
			next[k] = b
		}
	}
	return v.save(next)
}
