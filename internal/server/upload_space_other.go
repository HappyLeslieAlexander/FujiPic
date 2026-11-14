//go:build !linux && !darwin

package server

func uploadSpaceAvailable(string, int64) bool { return true }
