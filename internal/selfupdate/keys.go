package selfupdate

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
)

// ErrBadSignature is returned when no trusted key verifies a manifest.
var ErrBadSignature = errors.New("release manifest signature is invalid (no trusted key verifies it)")

// PublicKey is a trusted release key: the raw 32-byte Ed25519 public key.
// Its text form is standard base64 with padding (44 characters), the same
// form the Pi installer takes as --manifest-key / PRESET_MANIFEST_KEY.
type PublicKey ed25519.PublicKey

// ParsePublicKey decodes the base64 text form of a release public key.
// Surrounding white space is ignored.
func ParsePublicKey(s string) (PublicKey, error) {
	s = strings.TrimSpace(s)
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("release key %q: want base64 of a 32-byte Ed25519 public key", abbreviate(s))
	}
	return PublicKey(b), nil
}

// ParsePublicKeys parses a list of keys (key rotation: any one of them may
// sign). Entries may also be comma- or white-space-separated strings.
func ParsePublicKeys(list []string) ([]PublicKey, error) {
	var keys []PublicKey
	for _, item := range list {
		for _, f := range strings.FieldsFunc(item, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\r' || r == '\t' }) {
			k, err := ParsePublicKey(f)
			if err != nil {
				return nil, err
			}
			keys = append(keys, k)
		}
	}
	return keys, nil
}

// String returns the base64 text form.
func (k PublicKey) String() string { return base64.StdEncoding.EncodeToString(k) }

func abbreviate(s string) string {
	if len(s) > 12 {
		return s[:12] + "..."
	}
	return s
}

// GenerateKey creates a new release signing key pair.
func GenerateKey() (PublicKey, ed25519.PrivateKey, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	return PublicKey(pub), priv, nil
}

// MarshalPrivateKeyPEM encodes a private key as PKCS#8 PEM ("PRIVATE KEY"),
// readable by OpenSSL 3 (openssl pkey -in key.pem -text).
func MarshalPrivateKeyPEM(priv ed25519.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

// ParsePrivateKeyPEM decodes a PKCS#8 PEM Ed25519 private key.
func ParsePrivateKeyPEM(b []byte) (ed25519.PrivateKey, error) {
	blk, _ := pem.Decode(b)
	if blk == nil || blk.Type != "PRIVATE KEY" {
		return nil, errors.New("not a PEM \"PRIVATE KEY\" block")
	}
	k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		return nil, err
	}
	priv, ok := k.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("private key is %T, want Ed25519", k)
	}
	return priv, nil
}

// SignManifest returns the signature file content for manifest bytes:
// standard base64 (padded) of the 64-byte Ed25519 signature over the exact
// bytes, followed by a newline.
func SignManifest(priv ed25519.PrivateKey, manifest []byte) []byte {
	sig := ed25519.Sign(priv, manifest)
	return []byte(base64.StdEncoding.EncodeToString(sig) + "\n")
}

// DecodeSignature parses a signature file (base64, white space ignored).
func DecodeSignature(sigFile []byte) ([]byte, error) {
	clean := bytes.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\r', '\n':
			return -1
		}
		return r
	}, sigFile)
	sig, err := base64.StdEncoding.DecodeString(string(clean))
	if err != nil || len(sig) != ed25519.SignatureSize {
		return nil, errors.New("signature file: want base64 of a 64-byte Ed25519 signature")
	}
	return sig, nil
}

// VerifySignature checks sigFile over manifest with any of keys. It returns
// the index of the key that verified, or ErrBadSignature.
func VerifySignature(keys []PublicKey, manifest, sigFile []byte) (int, error) {
	if len(keys) == 0 {
		return -1, errors.New("no trusted release key configured")
	}
	sig, err := DecodeSignature(sigFile)
	if err != nil {
		return -1, fmt.Errorf("%w: %v", ErrBadSignature, err)
	}
	for i, k := range keys {
		if len(k) == ed25519.PublicKeySize && ed25519.Verify(ed25519.PublicKey(k), manifest, sig) {
			return i, nil
		}
	}
	return -1, ErrBadSignature
}

// VerifyManifest checks the signature first and only then parses the
// verified bytes. A manifest is never interpreted before it is verified.
func VerifyManifest(keys []PublicKey, manifest, sigFile []byte) (*Manifest, error) {
	if len(manifest) > MaxManifestBytes {
		return nil, errors.New("manifest too large")
	}
	if _, err := VerifySignature(keys, manifest, sigFile); err != nil {
		return nil, err
	}
	return ParseManifest(manifest)
}
