package wininstall

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ReceiptName is the install record kept in the install directory. The
// uninstaller removes exactly what it lists, nothing else.
const ReceiptName = "install-receipt.json"

// UninstallerName is the uninstaller written next to the program.
const UninstallerName = "uninstall.exe"

// Receipt records one installation.
type Receipt struct {
	Format    int       `json:"format"` // 1
	Product   Product   `json:"product"`
	Scope     string    `json:"scope"` // machine | user
	Dir       string    `json:"dir"`
	Installed time.Time `json:"installed"`
	// Files are install-relative, slash-separated (uninstall.exe and the
	// receipt itself are implied).
	Files []ReceiptFile `json:"files"`
	// Shortcuts and URL shortcuts created (absolute paths).
	Shortcuts []string `json:"shortcuts,omitempty"`
	// StartMenuDir is removed when empty after the shortcuts.
	StartMenuDir string `json:"startMenuDir,omitempty"`
	// Options chosen at install time.
	Service      bool   `json:"service,omitempty"`
	ServiceName  string `json:"serviceName,omitempty"`
	TrayRunValue string `json:"trayRunValue,omitempty"`
	Firewall     string `json:"firewallRule,omitempty"`
	Port         int    `json:"port,omitempty"`
	Desktop      bool   `json:"desktop,omitempty"`
	StartMenu    bool   `json:"startMenu,omitempty"`
	// DataDirs are the product's data folders, expanded at install time.
	DataDirs []string `json:"dataDirs,omitempty"`
}

// ReceiptFile is one installed file.
type ReceiptFile struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// ScopeValue returns the receipt's scope.
func (r *Receipt) ScopeValue() Scope {
	s, _ := ParseScope(r.Scope)
	return s
}

// ReadReceipt loads the receipt of an install directory.
func ReadReceipt(dir string) (*Receipt, error) {
	b, err := os.ReadFile(filepath.Join(dir, ReceiptName))
	if err != nil {
		return nil, err
	}
	var r Receipt
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("%s: %w", ReceiptName, err)
	}
	if r.Format != 1 {
		return nil, fmt.Errorf("%s: unsupported format %d", ReceiptName, r.Format)
	}
	if err := r.Product.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", ReceiptName, err)
	}
	for _, f := range r.Files {
		if !safeZipName(f.Path) {
			return nil, fmt.Errorf("%s: unsafe path %q", ReceiptName, f.Path)
		}
	}
	return &r, nil
}

// WriteReceipt stores the receipt atomically.
func WriteReceipt(dir string, r *Receipt) error {
	r.Format = 1
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	p := filepath.Join(dir, ReceiptName)
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
