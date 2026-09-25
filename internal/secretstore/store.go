// Package secretstore owns aninode's recoverable secret storage.
//
// Login passwords are deliberately not stored here: the web authentication
// layer stores only a verifier under the same /config/secrets security domain.
package secretstore

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"aninode/internal/atomicfile"
)

var componentRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

type ID struct {
	Namespace string
	Name      string
	Field     string
}

func DownloaderPassword(clientID string) ID {
	return ID{Namespace: "downloader", Name: clientID, Field: "password"}
}

func Integration(namespace, name, field string) ID {
	return ID{Namespace: namespace, Name: name, Field: field}
}

func (id ID) String() string {
	return id.Namespace + "/" + id.Name + "/" + id.Field
}

func (id ID) validate() error {
	for label, value := range map[string]string{"namespace": id.Namespace, "name": id.Name, "field": id.Field} {
		if !componentRE.MatchString(value) || value == "." || value == ".." {
			return fmt.Errorf("invalid secret %s %q", label, value)
		}
	}
	return nil
}

type Store struct {
	Root string
}

type envelope struct {
	Version    int    `json:"version"`
	Ciphertext string `json:"ciphertext"`
}

func (s Store) secretsRoot() (string, error) {
	if strings.TrimSpace(s.Root) == "" {
		return "", errors.New("configuration root is required")
	}
	return filepath.Join(s.Root, "secrets"), nil
}

func (s Store) path(id ID) (string, error) {
	if err := id.validate(); err != nil {
		return "", err
	}
	root, err := s.secretsRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "integrations", id.Namespace, id.Name, id.Field+".json"), nil
}

func secureDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	return os.Chmod(path, 0o700)
}

func (s Store) key(create bool) ([]byte, error) {
	root, err := s.secretsRoot()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(root, "master.key")
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) && create {
		if err := secureDir(root); err != nil {
			return nil, err
		}
		value := make([]byte, 32)
		if _, err := rand.Read(value); err != nil {
			return nil, err
		}
		if err := atomicfile.Create(path, value, 0o600); err != nil && !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
		info, err = os.Lstat(path)
	}
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, errors.New("secret master key is unavailable; restore /config/secrets from backup")
		}
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("secret master key must be a private regular file (0600)")
	}
	value, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(value) != 32 {
		return nil, errors.New("invalid secret master key")
	}
	return value, nil
}

func (s Store) aead(create bool) (cipher.AEAD, error) {
	value, err := s.key(create)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(value)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func (s Store) Put(id ID, value string) error {
	path, err := s.path(id)
	if err != nil {
		return err
	}
	c, err := s.aead(true)
	if err != nil {
		return err
	}
	nonce := make([]byte, c.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	sealed := c.Seal(nonce, nonce, []byte(value), []byte("aninode/secret/v1/"+id.String()))
	data, err := json.Marshal(envelope{Version: 1, Ciphertext: base64.RawStdEncoding.EncodeToString(sealed)})
	if err != nil {
		return err
	}
	if err := secureDir(filepath.Dir(path)); err != nil {
		return err
	}
	return atomicfile.Write(path, data, 0o600)
}

func (s Store) Exists(id ID) (bool, error) {
	path, err := s.path(id)
	if err != nil {
		return false, err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return false, errors.New("secret must be a private regular file (0600)")
	}
	return true, nil
}

func (s Store) Lookup(id ID) (string, bool, error) {
	path, err := s.path(id)
	if err != nil {
		return "", false, err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return "", false, errors.New("secret must be a private regular file (0600)")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false, err
	}
	var e envelope
	if err := json.Unmarshal(data, &e); err != nil || e.Version != 1 || strings.TrimSpace(e.Ciphertext) == "" {
		return "", false, errors.New("invalid encrypted secret")
	}
	c, err := s.aead(false)
	if err != nil {
		return "", false, err
	}
	sealed, err := base64.RawStdEncoding.DecodeString(e.Ciphertext)
	if err != nil || len(sealed) < c.NonceSize()+c.Overhead() {
		return "", false, errors.New("invalid encrypted secret")
	}
	plain, err := c.Open(nil, sealed[:c.NonceSize()], sealed[c.NonceSize():], []byte("aninode/secret/v1/"+id.String()))
	if err != nil {
		return "", false, errors.New("secret authentication failed")
	}
	return string(plain), true, nil
}

func (s Store) Delete(id ID) error {
	path, err := s.path(id)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
