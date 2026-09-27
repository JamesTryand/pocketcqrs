package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestBackgroundStopWaitsForLoopsToFinish(t *testing.T) {
	b := newBackground()
	var finished atomic.Int32
	for i := 0; i < 3; i++ {
		b.Go(func(ctx context.Context) {
			<-ctx.Done()
			time.Sleep(50 * time.Millisecond) // in-flight work after the stop
			finished.Add(1)
		})
	}
	if !b.Stop(5 * time.Second) {
		t.Fatal("Stop reported a timeout for loops that exit promptly")
	}
	if got := finished.Load(); got != 3 {
		t.Fatalf("Stop returned before every loop finished: %d/3", got)
	}
}

func TestBackgroundStopIsBounded(t *testing.T) {
	b := newBackground()
	release := make(chan struct{})
	defer close(release)
	b.Go(func(ctx context.Context) { <-release }) // ignores the stop

	start := time.Now()
	if b.Stop(100 * time.Millisecond) {
		t.Fatal("Stop reported success while a loop was still running")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Stop was not bounded by its timeout: took %s", elapsed)
	}
}
