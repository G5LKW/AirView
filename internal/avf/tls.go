package avf

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// TLS helpers (AVF.md §2): TLS 1.3 only, ALPN "avf/1", self-signed ECDSA
// P-256 server certificates verified by SPKI pin, operator certificates
// reloaded on change, and the exporter value bound into the HELLO_C
// signature.

// PinPrefix starts every server pin.
const PinPrefix = "sha256/"

// Self-signed certificate files inside the TLS directory
// (<data>/avf-tls/).
const (
	SelfSignedCertFile = "cert.pem"
	SelfSignedKeyFile  = "key.pem"
)

// selfSignedValidity is the lifetime of a generated certificate. Clients
// verify by pin, so expiry only matters to tools that inspect it; an
// expired certificate is re-issued for the same key (same pin).
const selfSignedValidity = 20 * 365 * 24 * time.Hour

// ErrPinMismatch is returned (wrapped) when the server's key does not match
// the configured pin. A client MUST NOT retry faster than its normal
// back-off.
var ErrPinMismatch = errors.New("avf: server key does not match the pinned key")

// ErrNoALPN is returned when the peer did not negotiate "avf/1".
var ErrNoALPN = errors.New("avf: peer did not negotiate ALPN avf/1")

// PinFromSPKI returns the pin of a DER SubjectPublicKeyInfo.
func PinFromSPKI(spki []byte) string {
	sum := sha256.Sum256(spki)
	return PinPrefix + base64.StdEncoding.EncodeToString(sum[:])
}

// CertPin returns the pin of a certificate's public key.
func CertPin(c *x509.Certificate) string { return PinFromSPKI(c.RawSubjectPublicKeyInfo) }

// TLSCertPin returns the pin of the leaf of a tls.Certificate.
func TLSCertPin(c *tls.Certificate) (string, error) {
	if c.Leaf != nil {
		return CertPin(c.Leaf), nil
	}
	if len(c.Certificate) == 0 {
		return "", errors.New("avf: certificate has no leaf")
	}
	leaf, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		return "", err
	}
	return CertPin(leaf), nil
}

// ParsePin decodes a pin "sha256/<base64>" into the 32-byte SPKI hash. It
// accepts the standard and URL-safe alphabets, padded or not.
func ParsePin(s string) ([32]byte, error) {
	var out [32]byte
	b64, ok := strings.CutPrefix(strings.TrimSpace(s), PinPrefix)
	if !ok {
		return out, fmt.Errorf("avf: pin %q does not start with %q", s, PinPrefix)
	}
	b64 = strings.TrimRight(b64, "=")
	var raw []byte
	var err error
	if strings.ContainsAny(b64, "-_") {
		raw, err = base64.RawURLEncoding.DecodeString(b64)
	} else {
		raw, err = base64.RawStdEncoding.DecodeString(b64)
	}
	if err != nil || len(raw) != 32 {
		return out, fmt.Errorf("avf: pin %q is not base64 of 32 bytes", s)
	}
	copy(out[:], raw)
	return out, nil
}

// CanonicalPin returns the canonical (standard, padded) form of a pin.
func CanonicalPin(s string) (string, error) {
	h, err := ParsePin(s)
	if err != nil {
		return "", err
	}
	return PinPrefix + base64.StdEncoding.EncodeToString(h[:]), nil
}

// GenerateSelfSigned creates an ECDSA P-256 key and a self-signed server
// certificate for hosts (DNS names or IP literals; may be empty) and returns
// both PEM encoded (certificate, PKCS#8 private key).
func GenerateSelfSigned(hosts []string) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	certPEM, err = selfSignedCert(key, hosts)
	if err != nil {
		return nil, nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return certPEM, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

func selfSignedCert(key *ecdsa.PrivateKey, hosts []string) ([]byte, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "AirView AVF server"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(selfSignedValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	for _, h := range hosts {
		if h = strings.TrimSpace(h); h == "" {
			continue
		}
		if ip := net.ParseIP(h); ip != nil {
			tpl.IPAddresses = append(tpl.IPAddresses, ip)
		} else {
			tpl.DNSNames = append(tpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), nil
}

// LoadOrCreateSelfSigned loads the self-signed server certificate from dir
// (normally <data>/avf-tls), creating the directory, key (mode 0600) and
// certificate on first use. An unreadable or expired certificate is
// re-issued for the existing key, so the pin stays the same. It returns the
// certificate and its pin.
func LoadOrCreateSelfSigned(dir string, hosts []string) (*tls.Certificate, string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, "", err
	}
	certPath := filepath.Join(dir, SelfSignedCertFile)
	keyPath := filepath.Join(dir, SelfSignedKeyFile)
	keyPEM, err := os.ReadFile(keyPath)
	if errors.Is(err, os.ErrNotExist) {
		certPEM, kp, err := GenerateSelfSigned(hosts)
		if err != nil {
			return nil, "", err
		}
		if err := writeFileAtomic(keyPath, kp, 0o600); err != nil {
			return nil, "", err
		}
		if err := writeFileAtomic(certPath, certPEM, 0o644); err != nil {
			return nil, "", err
		}
		return loadPair(certPEM, kp)
	}
	if err != nil {
		return nil, "", err
	}
	certPEM, err := os.ReadFile(certPath)
	if err == nil {
		if c, pin, err := loadPair(certPEM, keyPEM); err == nil && time.Now().Before(c.Leaf.NotAfter) {
			return c, pin, nil
		}
	}
	// Re-issue for the same key.
	block, _ := pem.Decode(keyPEM)
	if block == nil {
		return nil, "", fmt.Errorf("avf: %s is not PEM", keyPath)
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, "", fmt.Errorf("avf: %s: %w", keyPath, err)
	}
	ek, ok := k.(*ecdsa.PrivateKey)
	if !ok {
		return nil, "", fmt.Errorf("avf: %s is not an ECDSA key", keyPath)
	}
	certPEM, err = selfSignedCert(ek, hosts)
	if err != nil {
		return nil, "", err
	}
	if err := writeFileAtomic(certPath, certPEM, 0o644); err != nil {
		return nil, "", err
	}
	return loadPair(certPEM, keyPEM)
}

func loadPair(certPEM, keyPEM []byte) (*tls.Certificate, string, error) {
	c, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, "", err
	}
	if c.Leaf == nil {
		if c.Leaf, err = x509.ParseCertificate(c.Certificate[0]); err != nil {
			return nil, "", err
		}
	}
	return &c, CertPin(c.Leaf), nil
}

// writeFileAtomic writes data to a temporary file in the same directory and
// renames it over path.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(name, mode)
	}
	if werr == nil {
		werr = os.Rename(name, path)
	}
	if werr != nil {
		_ = os.Remove(name)
	}
	return werr
}

// CertReloader serves an operator certificate (Aggregator.TLSCert/TLSKey)
// and reloads it when either file's modification time changes (checked at
// most once per second). A failed reload keeps the previous certificate.
type CertReloader struct {
	certFile, keyFile string

	mu        sync.Mutex
	cert      *tls.Certificate
	certMod   time.Time
	keyMod    time.Time
	lastCheck time.Time
	lastErr   error
}

// NewCertReloader loads the pair once and returns the reloader.
func NewCertReloader(certFile, keyFile string) (*CertReloader, error) {
	r := &CertReloader{certFile: certFile, keyFile: keyFile}
	if err := r.reload(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *CertReloader) reload() error {
	ci, err := os.Stat(r.certFile)
	if err != nil {
		return err
	}
	ki, err := os.Stat(r.keyFile)
	if err != nil {
		return err
	}
	if r.cert != nil && ci.ModTime().Equal(r.certMod) && ki.ModTime().Equal(r.keyMod) {
		return nil
	}
	c, err := tls.LoadX509KeyPair(r.certFile, r.keyFile)
	if err != nil {
		return err
	}
	if c.Leaf == nil {
		if c.Leaf, err = x509.ParseCertificate(c.Certificate[0]); err != nil {
			return err
		}
	}
	r.cert, r.certMod, r.keyMod = &c, ci.ModTime(), ki.ModTime()
	return nil
}

// GetCertificate implements tls.Config.GetCertificate.
func (r *CertReloader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if now := time.Now(); now.Sub(r.lastCheck) >= time.Second {
		r.lastCheck = now
		r.lastErr = r.reload()
	}
	return r.cert, nil
}

// Current returns the certificate in use and the error of the last reload
// attempt (nil when it succeeded or nothing changed).
func (r *CertReloader) Current() (*tls.Certificate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cert, r.lastErr
}

// StaticCertificate returns a GetCertificate function serving c.
func StaticCertificate(c *tls.Certificate) func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return c, nil }
}

// ServerTLSConfig returns the server configuration: TLS 1.3 only, ALPN
// "avf/1" mandatory (a client that does not offer it is refused during the
// handshake), no client certificates.
func ServerTLSConfig(getCert func(*tls.ClientHelloInfo) (*tls.Certificate, error)) *tls.Config {
	cfg := &tls.Config{
		MinVersion:     tls.VersionTLS13,
		MaxVersion:     tls.VersionTLS13,
		NextProtos:     []string{ALPN},
		GetCertificate: getCert,
		ClientAuth:     tls.NoClientCert,
	}
	cfg.GetConfigForClient = func(chi *tls.ClientHelloInfo) (*tls.Config, error) {
		// Go's server proceeds without ALPN when the client offers none;
		// AVF/1 requires it, so refuse here (the handshake is aborted).
		if !slices.Contains(chi.SupportedProtos, ALPN) {
			return nil, ErrNoALPN
		}
		return nil, nil
	}
	return cfg
}

// ClientTLSConfig returns the client configuration for host (a name or IP
// literal; SNI is sent only for names). With pin == "" the server is
// verified against roots (nil = system roots) and host; otherwise chain and
// name checks are skipped and the leaf's SPKI hash must equal the pin.
func ClientTLSConfig(host, pin string, roots *x509.CertPool) (*tls.Config, error) {
	cfg := &tls.Config{
		MinVersion: tls.VersionTLS13,
		MaxVersion: tls.VersionTLS13,
		NextProtos: []string{ALPN},
		RootCAs:    roots,
	}
	h := host
	if net.ParseIP(strings.Trim(h, "[]")) == nil {
		cfg.ServerName = h
	}
	cfg.VerifyConnection = func(cs tls.ConnectionState) error {
		if cs.NegotiatedProtocol != ALPN {
			return ErrNoALPN
		}
		return nil
	}
	if pin == "" {
		if cfg.ServerName == "" {
			// An IP literal: verify the certificate's IP SANs.
			cfg.ServerName = strings.Trim(h, "[]")
		}
		return cfg, nil
	}
	want, err := ParsePin(pin)
	if err != nil {
		return nil, err
	}
	cfg.InsecureSkipVerify = true // replaced by the pin check below
	cfg.VerifyPeerCertificate = func(raw [][]byte, _ [][]*x509.Certificate) error {
		if len(raw) == 0 {
			return ErrPinMismatch
		}
		leaf, err := x509.ParseCertificate(raw[0])
		if err != nil {
			return fmt.Errorf("%w: %v", ErrPinMismatch, err)
		}
		got := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
		if subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
			return fmt.Errorf("%w (got %s)", ErrPinMismatch, PinFromSPKI(leaf.RawSubjectPublicKeyInfo))
		}
		return nil
	}
	return cfg, nil
}

// ExportKeyingMaterial returns the AVF exporter value of a TLS 1.3
// connection: label "EXPORTER-AirView-AVF1", empty context, 32 bytes.
func ExportKeyingMaterial(cs *tls.ConnectionState) ([]byte, error) {
	if cs.Version != tls.VersionTLS13 {
		return nil, errors.New("avf: connection is not TLS 1.3")
	}
	if !cs.HandshakeComplete {
		return nil, errors.New("avf: TLS handshake not complete")
	}
	return cs.ExportKeyingMaterial(ExporterLabel, nil, ExporterLen)
}
