package avf

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"airview/internal/model"
)

// IdentityFile is the feeder key file name (in the feeder's data
// directory).
const IdentityFile = "identity.key"

// Identity is a feeder's Ed25519 key pair.
type Identity struct {
	Priv ed25519.PrivateKey
}

// Public returns the 32-byte public key.
func (id Identity) Public() ed25519.PublicKey { return id.Priv.Public().(ed25519.PublicKey) }

// FeederID returns the feeder ID derived from the public key.
func (id Identity) FeederID() string { return model.FeederIDFromPub(id.Public()) }

// NewIdentity generates a fresh key pair.
func NewIdentity() (Identity, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return Identity{}, err
	}
	return Identity{Priv: priv}, nil
}

// MarshalIdentity encodes the key as PEM (PKCS#8 "PRIVATE KEY").
func MarshalIdentity(id Identity) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(id.Priv)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

// ParseIdentity decodes a PEM key written by MarshalIdentity.
func ParseIdentity(b []byte) (Identity, error) {
	block, _ := pem.Decode(b)
	if block == nil {
		return Identity{}, errors.New("avf: identity key is not PEM")
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return Identity{}, fmt.Errorf("avf: identity key: %w", err)
	}
	priv, ok := k.(ed25519.PrivateKey)
	if !ok {
		return Identity{}, errors.New("avf: identity key is not Ed25519")
	}
	return Identity{Priv: priv}, nil
}

// LoadOrCreateIdentity loads the key from path, creating it (mode 0600, in
// a 0700 directory) when it does not exist. created reports a new key.
func LoadOrCreateIdentity(path string) (id Identity, created bool, err error) {
	b, err := os.ReadFile(path)
	if err == nil {
		id, err = ParseIdentity(b)
		return id, false, err
	}
	if !errors.Is(err, os.ErrNotExist) {
		return Identity{}, false, err
	}
	id, err = ReplaceIdentity(path)
	return id, err == nil, err
}

// ReplaceIdentity writes a fresh key to path, replacing any existing one
// ("airview-feeder new-identity").
func ReplaceIdentity(path string) (Identity, error) {
	id, err := NewIdentity()
	if err != nil {
		return Identity{}, err
	}
	pemBytes, err := MarshalIdentity(id)
	if err != nil {
		return Identity{}, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return Identity{}, err
	}
	if err := writeFileAtomic(path, pemBytes, 0o600); err != nil {
		return Identity{}, err
	}
	return id, nil
}
