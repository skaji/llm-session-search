//go:build !darwin || !arm64

package search

func disclaimDaemon() error { return nil }
