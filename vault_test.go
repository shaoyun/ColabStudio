package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/egoist/mygo"
)

type countingKeyStore struct {
	key           []byte
	reads, writes int
	denied        bool
}

func (s *countingKeyStore) Get(string) ([]byte, error) {
	s.reads++
	if s.denied {
		return nil, errors.New("denied")
	}
	return append([]byte(nil), s.key...), nil
}
func (s *countingKeyStore) Set(_ string, b []byte) error {
	s.writes++
	if s.denied {
		return errors.New("denied")
	}
	s.key = append([]byte(nil), b...)
	return nil
}
func TestVaultOneKeychainOperationPerProcess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.enc")
	backend := &countingKeyStore{}
	v := newCredentialVault(path, backend)
	for _, k := range []string{"client-secret", "refresh-token", "access-token"} {
		if e := v.Set(k, []byte("private-"+k)); e != nil {
			t.Fatal(e)
		}
	}
	for i := 0; i < 20; i++ {
		for _, k := range []string{"client-secret", "refresh-token", "access-token"} {
			if _, e := v.Get(k); e != nil {
				t.Fatal(e)
			}
		}
		if e := v.Set("access-token", []byte("new-access-secret")); e != nil {
			t.Fatal(e)
		}
	}
	if backend.reads != 0 || backend.writes != 1 {
		t.Fatalf("new session: reads=%d writes=%d", backend.reads, backend.writes)
	}
	b, _ := os.ReadFile(path)
	if bytes.Contains(b, []byte("private-")) || bytes.Contains(b, []byte("new-access-secret")) {
		t.Fatal("credentials stored in plaintext")
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatal("incorrect file permissions")
	}
	restarted := newCredentialVault(path, backend)
	for i := 0; i < 20; i++ {
		if _, e := restarted.Get("refresh-token"); e != nil {
			t.Fatal(e)
		}
		if e := restarted.Set("access-token", []byte("renewed")); e != nil {
			t.Fatal(e)
		}
	}
	if backend.reads != 1 || backend.writes != 1 {
		t.Fatalf("reopened session: reads=%d writes=%d", backend.reads, backend.writes)
	}
	if e := restarted.Delete("refresh-token"); e != nil {
		t.Fatal(e)
	}
	if _, e := restarted.Get("refresh-token"); !errors.Is(e, mygo.ErrSecretNotFound) {
		t.Fatal("logout did not remove credential")
	}
	if backend.reads != 1 || backend.writes != 1 {
		t.Fatal("logout reopened Keychain")
	}
}
func TestVaultDeniedAccessDoesNotPromptAgain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.enc")
	backend := &countingKeyStore{}
	v := newCredentialVault(path, backend)
	if e := v.Set("refresh-token", []byte("private")); e != nil {
		t.Fatal(e)
	}
	backend.denied = true
	restarted := newCredentialVault(path, backend)
	for i := 0; i < 10; i++ {
		if _, e := restarted.Get("refresh-token"); e == nil {
			t.Fatal("expected denied read")
		}
		if e := restarted.Set("access-token", []byte("private")); e == nil {
			t.Fatal("expected denied write")
		}
	}
	if backend.reads != 1 || backend.writes != 1 {
		t.Fatalf("repeated prompts: reads=%d writes=%d", backend.reads, backend.writes)
	}
}
func TestVaultTamperingFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.enc")
	backend := &countingKeyStore{}
	v := newCredentialVault(path, backend)
	if e := v.Set("refresh-token", []byte("private")); e != nil {
		t.Fatal(e)
	}
	b, _ := os.ReadFile(path)
	b[len(b)-1] ^= 1
	os.WriteFile(path, b, 0600)
	v = newCredentialVault(path, backend)
	if _, e := v.Get("refresh-token"); e == nil {
		t.Fatal("accepted modified ciphertext")
	}
}
func TestVaultStatusWithoutCredentialsDoesNotReadKeychain(t *testing.T) {
	backend := &countingKeyStore{}
	v := newCredentialVault(filepath.Join(t.TempDir(), "missing.enc"), backend)
	for i := 0; i < 5; i++ {
		_, _ = v.Get("refresh-token")
	}
	if backend.reads != 0 || backend.writes != 0 {
		t.Fatal("unconnected app touched Keychain")
	}
}
