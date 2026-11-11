package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"fujipic/internal/model"
)

func TestInviteLimitsAndConcurrentRegistration(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	for _, tc := range []struct {
		code      string
		max       int64
		unlimited bool
		successes int
	}{
		{"default", 0, false, 1}, {"limited", 3, false, 3}, {"unlimited", 0, true, 10},
	} {
		t.Run(tc.code, func(t *testing.T) {
			if err := s.CreateInvite(ctx, &model.Invite{Code: tc.code, MaxUses: tc.max, Unlimited: tc.unlimited}); err != nil {
				t.Fatal(err)
			}
			var wg sync.WaitGroup
			results := make(chan error, 10)
			for i := 0; i < 10; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					u := &model.User{ID: fmt.Sprintf("%s-%d", tc.code, i), Email: fmt.Sprintf("%s-%d@example.test", tc.code, i), PasswordHash: "h", Role: "user", Status: "active", QuotaBytes: 100}
					results <- s.CreateUserWithInvite(ctx, u, tc.code)
				}(i)
			}
			wg.Wait()
			close(results)
			successes := 0
			for err := range results {
				if err == nil {
					successes++
				} else if !errors.Is(err, ErrInviteInvalid) {
					t.Fatal(err)
				}
			}
			if successes != tc.successes {
				t.Fatalf("successful registrations=%d want=%d", successes, tc.successes)
			}
			var count, users int
			if err := s.DB().QueryRow(`SELECT use_count FROM invites WHERE code=?`, tc.code).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if err := s.DB().QueryRow(`SELECT count(*) FROM users WHERE id LIKE ?`, tc.code+"-%").Scan(&users); err != nil {
				t.Fatal(err)
			}
			if count != successes || users != successes {
				t.Fatal("usage or user transaction mismatch")
			}
		})
	}
}

func TestInviteRollbackAndUnlimitedExpiry(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u := newUser(t, s, "existing@example.test", "user")
	if err := s.CreateInvite(ctx, &model.Invite{Code: "limited", MaxUses: 2}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateUserWithInvite(ctx, u, "limited"); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate user: %v", err)
	}
	var count int
	if err := s.DB().QueryRow(`SELECT use_count FROM invites WHERE code='limited'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("failed registration consumed invite: %d %v", count, err)
	}
	past := time.Now().Add(-time.Hour).Unix()
	if err := s.CreateInvite(ctx, &model.Invite{Code: "expired", Unlimited: true, ExpiresAt: &past}); err != nil {
		t.Fatal(err)
	}
	if err := s.ValidateInvite(ctx, "expired"); !errors.Is(err, ErrInviteInvalid) {
		t.Fatal("expired unlimited invite valid")
	}
	if err := s.ConsumeInvite(ctx, "expired", u.ID); !errors.Is(err, ErrInviteInvalid) {
		t.Fatal("expired unlimited invite consumed")
	}
}
