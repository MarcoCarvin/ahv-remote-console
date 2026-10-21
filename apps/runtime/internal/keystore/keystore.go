// Package keystore stores small secrets (refresh tokens, pinned certs, etc.)
// in OS-native secure storage:
//
//   * Windows  — Credential Manager via wincred (zalando/go-keyring).
//   * macOS    — Keychain (zalando/go-keyring).
//   * Linux    — Secret Service (libsecret) via dbus.
//
// There is no file fallback. On any platform where the OS keyring service is
// unavailable, the keystore fails closed: operations return a hard error and
// the runtime refuses to start. Secrets only ever live in the OS keychain.
package keystore

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/zalando/go-keyring"
)

type Store interface {
	Get(key string) ([]byte, error)
	Set(key string, value []byte) error
	Delete(key string) error
	List(prefix string) ([]string, error)
}

type Options struct {
	ServiceName string
	Logger      *slog.Logger
}

func Open(opts Options) (Store, error) {
	if opts.ServiceName == "" {
		return nil, errors.New("keystore: ServiceName required")
	}
	s := &osStore{service: opts.ServiceName, log: opts.Logger}
	if err := s.selfTest(); err != nil {
		return nil, fmt.Errorf("keystore self-test failed: %w", err)
	}
	return s, nil
}

type osStore struct {
	service string
	log     *slog.Logger
}

func (s *osStore) selfTest() error {
	const key = "__selftest__"
	if err := keyring.Set(s.service, key, "ok"); err != nil {
		return err
	}
	got, err := keyring.Get(s.service, key)
	if err != nil {
		return err
	}
	if got != "ok" {
		return errors.New("selftest mismatch")
	}
	return keyring.Delete(s.service, key)
}

func (s *osStore) Get(key string) ([]byte, error) {
	v, err := keyring.Get(s.service, key)
	if err != nil {
		if errors.Is(err, keyring.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return []byte(v), nil
}

func (s *osStore) Set(key string, value []byte) error {
	return keyring.Set(s.service, key, string(value))
}

func (s *osStore) Delete(key string) error {
	err := keyring.Delete(s.service, key)
	if errors.Is(err, keyring.ErrNotFound) {
		return nil
	}
	return err
}

func (s *osStore) List(_ string) ([]string, error) {
	// go-keyring does not expose enumeration; callers should track keys
	// themselves in config. We return an empty list rather than an error so
	// callers can branch on capability.
	return nil, nil
}

var ErrNotFound = errors.New("keystore: not found")
