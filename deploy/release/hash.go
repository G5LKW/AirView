package main

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
)

// hashLocal returns the SHA-256 (hex) and size of a file.
func hashLocal(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), n, nil
}
