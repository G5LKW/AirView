package main

import (
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"airview/internal/config"
	"airview/internal/engine"
)

// envGenerateAdmin turns on genAdminPassword (the Docker image sets it):
// a headless standalone server without an admin password generates one on
// its first start (SEC-R2).
const envGenerateAdmin = "AIRVIEW_GENERATE_ADMIN_PASSWORD"

// adminPasswordFile is the file (in the data directory, mode 0600) that
// holds a generated admin password.
const adminPasswordFile = "admin-password.txt"

// genAdminPassword sets a random admin password when AIRVIEW_GENERATE_ADMIN_PASSWORD
// is "1"/"true", the server is standalone and has none: the password is
// stored hashed in config.json and in plain text in
// <data>/admin-password.txt (readable by the service account only), whose
// path is logged. A container's port publishing can make every client look
// like a private-network peer (the bridge gateway), so the image must not
// rely on the LAN-is-admin default.
func genAdminPassword(logger *slog.Logger, eng *engine.Engine, dir string) {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(envGenerateAdmin))) {
	case "1", "true", "yes", "on":
	default:
		return
	}
	path := filepath.Join(dir, adminPasswordFile)
	if eng.Aggregator() {
		return
	}
	if h := eng.EffectiveConfig().HTTP.AdminPasswordHash; h != "" {
		// The owner changed the generated password: its file goes (the
		// image has no shell to delete it with).
		if b, err := os.ReadFile(path); err == nil && !config.CheckPassword(h, strings.TrimSpace(string(b))) {
			_ = os.Remove(path)
		}
		return
	}
	pw, err := randomPassword()
	if err != nil {
		logger.Error("generating the admin password", "err", err)
		return
	}
	if err := os.WriteFile(path, []byte(pw+"\n"), 0o600); err != nil {
		logger.Error("writing the generated admin password", "file", path, "err", err)
		return
	}
	cfg := eng.Config().Get()
	cfg.HTTP.AdminPasswordHash = "plain:" + pw
	if _, _, err := eng.ApplyConfig(cfg); err != nil {
		_ = os.Remove(path)
		logger.Error("setting the generated admin password", "err", err)
		return
	}
	logger.Warn("no admin password was set: generated one", "file", path,
		"hint", "read it with: docker cp <container>:"+path+" - (change it in Settings: the file is then removed at the next start)")
}

// randomPassword is 20 base32 characters (100 bits).
func randomPassword() (string, error) {
	var b [13]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	s := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:])
	if len(s) < 20 {
		return "", fmt.Errorf("short password")
	}
	return strings.ToLower(s[:20]), nil
}
