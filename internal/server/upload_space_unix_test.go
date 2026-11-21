//go:build linux || darwin

package server

import (
	"math"
	"testing"
)

func TestUploadDiskAdmissionRejectsImpossibleReservation(t *testing.T) {
	if uploadSpaceAvailable(t.TempDir(), math.MaxInt64) {
		t.Fatal("impossible disk reservation accepted")
	}
}
