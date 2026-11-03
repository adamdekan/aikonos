// broker/cmd/broker/singleton_test.go
package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakeLocker implements singletonLocker for unit tests without a live Postgres.
type fakeLocker struct {
	locked  bool
	err     error
	pingErr error
}

func (f *fakeLocker) Ping(ctx context.Context) error { return f.pingErr }

func (f *fakeLocker) TryLock(ctx context.Context, key int64) (bool, error) {
	return f.locked, f.err
}

func TestAcquireSingletonLock(t *testing.T) {
	tests := []struct {
		name    string
		locker  *fakeLocker
		wantErr bool
	}{
		{
			name:    "TryLock returns (false, nil) — contention, error",
			locker:  &fakeLocker{locked: false, err: nil},
			wantErr: true,
		},
		{
			name:    "TryLock returns (true, nil) — acquired, no error",
			locker:  &fakeLocker{locked: true, err: nil},
			wantErr: false,
		},
		{
			name:    "TryLock returns an error — propagated",
			locker:  &fakeLocker{locked: false, err: errors.New("connection reset")},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := acquireSingletonLock(context.Background(), tc.locker, singletonLockKey)
			if tc.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected no error, got: %v", err)
			}
		})
	}
}

func TestWatchSingletonLockExitsOnFailedPing(t *testing.T) {
	exited := make(chan error, 1)
	go watchSingletonLock(context.Background(), &fakeLocker{pingErr: errors.New("conn closed")},
		time.Millisecond, time.Second, func(err error) { exited <- err })
	select {
	case <-exited:
	case <-time.After(2 * time.Second):
		t.Fatal("exit not called after failed ping")
	}
}
