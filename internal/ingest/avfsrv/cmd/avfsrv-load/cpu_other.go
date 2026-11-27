//go:build !windows && !unix

package main

func processCPU() float64 { return 0 }
