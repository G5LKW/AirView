//go:build !linux

package main

// owner is a no-op outside Linux (the program only runs on the Pi; other
// builds exist for tests).
type owner struct{}

func statOwner(string) owner { return owner{} }

func (owner) restore(string) error { return nil }
