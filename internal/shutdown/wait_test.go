package shutdown

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestWaitReturnsWhenGroupCompletes(t *testing.T) {
	var wg sync.WaitGroup
	wg.Add(1)

	go func() {
		defer wg.Done()
		time.Sleep(5 * time.Millisecond)
	}()

	if err := Wait(context.Background(), &wg); err != nil {
		t.Fatalf("Wait returned error: %v", err)
	}
}

func TestWaitReturnsContextErrorWhenDeadlineExpires(t *testing.T) {
	var wg sync.WaitGroup
	wg.Add(1)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()

	if err := Wait(ctx, &wg); err == nil {
		t.Fatal("Wait returned nil, want context deadline error")
	}

	wg.Done()
}
