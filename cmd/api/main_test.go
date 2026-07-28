package main

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestRunGracefulShutdownOrdersHTTPWorkersBusBeforeDB(t *testing.T) {
	var order []string
	httpStopped := false
	busClosed := false

	result := runGracefulShutdown(context.Background(), gracefulShutdownDeps{
		ShutdownHTTP: func(context.Context) error {
			order = append(order, "http")
			httpStopped = true
			return nil
		},
		StopWorkers: func() {
			order = append(order, "workers-stop")
		},
		WaitWorkers: func(context.Context) error {
			order = append(order, "workers-wait")
			return nil
		},
		CloseBus: func(context.Context) error {
			if !httpStopped {
				t.Fatal("bus closed before HTTP listener stopped accepting connections")
			}
			order = append(order, "bus")
			busClosed = true
			return nil
		},
		CloseDB: func(context.Context) error {
			if !httpStopped {
				t.Fatal("database closed before HTTP listener stopped accepting connections")
			}
			if !busClosed {
				t.Fatal("database closed before bus consumers drained/unsubscribed")
			}
			order = append(order, "db")
			return nil
		},
	})

	if result.Err != nil {
		t.Fatalf("runGracefulShutdown returned error: %v", result.Err)
	}
	if result.WorkerErr != nil {
		t.Fatalf("runGracefulShutdown returned worker error: %v", result.WorkerErr)
	}

	want := []string{"http", "workers-stop", "workers-wait", "bus", "db"}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("shutdown order = %v, want %v", order, want)
	}
}

func TestRunGracefulShutdownKeepsDBOpenUntilHTTPDrainCompletes(t *testing.T) {
	requestDrained := false

	result := runGracefulShutdown(context.Background(), gracefulShutdownDeps{
		ShutdownHTTP: func(context.Context) error {
			requestDrained = true
			return nil
		},
		CloseDB: func(context.Context) error {
			if !requestDrained {
				t.Fatal("database closed while HTTP shutdown was still draining in-flight work")
			}
			return nil
		},
	})

	if result.Err != nil {
		t.Fatalf("runGracefulShutdown returned error: %v", result.Err)
	}
}

func TestRunGracefulShutdownDoesNotCloseDependenciesWhenHTTPShutdownFails(t *testing.T) {
	httpErr := errors.New("listener still draining")

	result := runGracefulShutdown(context.Background(), gracefulShutdownDeps{
		ShutdownHTTP: func(context.Context) error {
			return httpErr
		},
		CloseBus: func(context.Context) error {
			t.Fatal("bus closed after HTTP shutdown failed")
			return nil
		},
		CloseDB: func(context.Context) error {
			t.Fatal("database closed after HTTP shutdown failed")
			return nil
		},
	})

	if !errors.Is(result.Err, httpErr) {
		t.Fatalf("shutdown error = %v, want wrapped HTTP error", result.Err)
	}
	if result.WorkerErr != nil {
		t.Fatalf("worker error = %v, want nil", result.WorkerErr)
	}
}

func TestGracefulShutdownRunnerRunsOnceForRepeatedSignals(t *testing.T) {
	var calls int
	runner := gracefulShutdownRunner{
		deps: gracefulShutdownDeps{
			ShutdownHTTP: func(context.Context) error {
				calls++
				return nil
			},
			CloseBus: func(context.Context) error {
				calls++
				return nil
			},
			CloseDB: func(context.Context) error {
				calls++
				return nil
			},
		},
	}

	first := runner.Run(context.Background())
	second := runner.Run(context.Background())

	if first.Err != nil || first.WorkerErr != nil {
		t.Fatalf("first shutdown returned unexpected result: %+v", first)
	}
	if second.Err != nil || second.WorkerErr != nil {
		t.Fatalf("second shutdown returned unexpected result: %+v", second)
	}
	if calls != 3 {
		t.Fatalf("shutdown callbacks called %d times, want 3", calls)
	}
}

func TestRunGracefulShutdownReportsDependencyCloseErrors(t *testing.T) {
	busErr := errors.New("bus drain failed")
	dbErr := errors.New("database close failed")
	var order []string

	result := runGracefulShutdown(context.Background(), gracefulShutdownDeps{
		CloseBus: func(context.Context) error {
			order = append(order, "bus")
			return busErr
		},
		CloseDB: func(context.Context) error {
			order = append(order, "db")
			return dbErr
		},
	})

	if !errors.Is(result.Err, busErr) {
		t.Fatalf("shutdown error %v does not wrap bus error", result.Err)
	}
	if !errors.Is(result.Err, dbErr) {
		t.Fatalf("shutdown error %v does not wrap database error", result.Err)
	}
	if !strings.Contains(result.Err.Error(), "close bus") || !strings.Contains(result.Err.Error(), "close database") {
		t.Fatalf("shutdown error lacks dependency context: %v", result.Err)
	}
	want := []string{"bus", "db"}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("close order = %v, want %v", order, want)
	}
}

func TestRunGracefulShutdownKeepsWorkerWaitTimeoutNonFatal(t *testing.T) {
	waitErr := context.DeadlineExceeded
	dbClosed := false

	result := runGracefulShutdown(context.Background(), gracefulShutdownDeps{
		WaitWorkers: func(context.Context) error {
			return waitErr
		},
		CloseDB: func(context.Context) error {
			dbClosed = true
			return nil
		},
	})

	if result.Err != nil {
		t.Fatalf("worker wait timeout should not be treated as fatal shutdown error: %v", result.Err)
	}
	if !errors.Is(result.WorkerErr, waitErr) {
		t.Fatalf("worker error = %v, want %v", result.WorkerErr, waitErr)
	}
	if !dbClosed {
		t.Fatal("database close was skipped after worker wait timeout")
	}
}

func TestAwaitListenerStartup_ReturnsListenerErrorPromptly(t *testing.T) {
	errCh := make(chan error, 1)
	wantErr := fmt.Errorf("listen tcp :8080: bind: address already in use")
	errCh <- wantErr

	start := time.Now()
	err := awaitListenerStartup(errCh, 2*time.Second)
	elapsed := time.Since(start)

	if !errors.Is(err, wantErr) {
		t.Fatalf("awaitListenerStartup() = %v, want %v", err, wantErr)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("awaitListenerStartup() took %v to return a queued error, want near-immediate return well under the grace window", elapsed)
	}
}

func TestAwaitListenerStartup_ReturnsNilOnceGraceElapsesWithoutError(t *testing.T) {
	errCh := make(chan error, 1)

	err := awaitListenerStartup(errCh, 20*time.Millisecond)

	if err != nil {
		t.Fatalf("awaitListenerStartup() = %v, want nil once the grace window elapses without a listener error", err)
	}
}
