package uplink

import (
	"bufio"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"airview/internal/avf"
	"airview/internal/model"
)

// Identity is the feeder key pair and UUID of one installation (DESIGN §3).
// All uplinks of an installation share it. The key file format is the one
// of internal/avf (PEM PKCS#8 Ed25519, mode 0600).
type Identity struct {
	AVF      avf.Identity
	FeederID string // model.FeederIDFromPub(public key)
	UUID     string // normalised UUIDv4 (for compat links and support)
}

// Public returns the Ed25519 public key.
func (id *Identity) Public() ed25519.PublicKey { return id.AVF.Public() }

// File names inside the state directory.
const (
	IdentityFile = avf.IdentityFile
	UUIDFile     = "uuid"
)

// NewMemoryIdentity returns a fresh identity that is not stored anywhere.
func NewMemoryIdentity() (*Identity, error) {
	k, err := avf.NewIdentity()
	if err != nil {
		return nil, err
	}
	u, _ := model.NormalizeUUID(newUUID())
	return &Identity{AVF: k, FeederID: k.FeederID(), UUID: u}, nil
}

// LoadOrCreateIdentity reads <dir>/identity.key and <dir>/uuid, creating
// them on first use (key file mode 0600). dir "" returns a memory identity.
func LoadOrCreateIdentity(dir string) (*Identity, error) {
	if dir == "" {
		return NewMemoryIdentity()
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	kp := filepath.Join(dir, IdentityFile)
	k, _, err := avf.LoadOrCreateIdentity(kp)
	if err != nil {
		return nil, fmt.Errorf("uplink: %s: %w", kp, err)
	}
	checkKeyMode(kp)
	return withUUID(dir, k)
}

// NewIdentity replaces the key of dir with a fresh one (airview-feeder
// new-identity, used after a "cloned_feeder" refusal). The UUID is kept.
func NewIdentity(dir string) (*Identity, error) {
	if dir == "" {
		return NewMemoryIdentity()
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	k, err := avf.ReplaceIdentity(filepath.Join(dir, IdentityFile))
	if err != nil {
		return nil, err
	}
	return withUUID(dir, k)
}

// withUUID completes an identity with the UUID file of dir (created on
// first use).
func withUUID(dir string, k avf.Identity) (*Identity, error) {
	id := &Identity{AVF: k, FeederID: k.FeederID()}
	up := filepath.Join(dir, UUIDFile)
	if b, err := os.ReadFile(up); err == nil {
		if u, ok := model.NormalizeUUID(strings.TrimSpace(string(b))); ok {
			id.UUID = u
		}
	}
	if id.UUID == "" {
		u := newUUID()
		if err := writeFileAtomic(up, []byte(u+"\n"), 0o644); err != nil {
			return nil, err
		}
		id.UUID, _ = model.NormalizeUUID(u)
	}
	return id, nil
}

// checkKeyMode tightens a key file that is readable by others (Unix only;
// on Windows the installer restricts the ACL of the data directory).
func checkKeyMode(path string) {
	if runtime.GOOS == "windows" {
		return
	}
	if st, err := os.Stat(path); err == nil && st.Mode().Perm()&0o077 != 0 {
		_ = os.Chmod(path, 0o600)
	}
}
func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0F | 0x40
	b[8] = b[8]&0x3F | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// writeFileAtomic writes via a temporary file and rename.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	w := bufio.NewWriter(tmp)
	_, err = w.Write(data)
	if err == nil {
		err = w.Flush()
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(name, perm)
	}
	if err == nil {
		err = os.Rename(name, path)
	}
	if err != nil {
		_ = os.Remove(name)
	}
	return err
}

// RootOnSD reports whether the root filesystem is mounted from an mmcblk
// device (an SD card on a Raspberry Pi). Always false off Linux.
func RootOnSD() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	b, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return false
	}
	return rootOnSD(string(b))
}

func rootOnSD(mounts string) bool {
	sd := false
	for _, line := range strings.Split(mounts, "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[1] == "/" {
			// the last entry for "/" wins (overlays)
			sd = strings.HasPrefix(f[0], "/dev/mmcblk")
		}
	}
	return sd
}
