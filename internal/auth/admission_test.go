package auth

import (
	"context"
	"errors"
	"testing"
)

func TestPasswordAdmissionCoversEveryPrimitive(t *testing.T) {
	if err := passwordGate.Acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer passwordGate.Release()
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

func TestPasswordAdmissionHonorsCanceledRequest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := HashPasswordContext(ctx, "password123"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := VerifyPasswordContext(ctx, "password123", dummyHash); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
