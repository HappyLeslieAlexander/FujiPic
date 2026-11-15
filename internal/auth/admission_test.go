package auth

import (
	"errors"
	"testing"
)

func TestPasswordAdmissionCoversEveryPrimitive(t *testing.T) {
	passwordSlot <- struct{}{}
	defer func() { <-passwordSlot }()
	if _, err := HashPassword("password123"); !errors.Is(err, ErrBusy) {
		t.Fatal(err)
	}
	if _, err := VerifyPassword("password123", dummyHash); !errors.Is(err, ErrBusy) {
		t.Fatal(err)
	}
	if err := DummyVerify("password123"); !errors.Is(err, ErrBusy) {
		t.Fatal(err)
	}
}
