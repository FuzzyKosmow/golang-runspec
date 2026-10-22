package snmp

import (
	"context"
	"sync"
)

type targetMemoryKey struct{}

type memTarget struct {
	ip        string
	community string
}

// targetMemory holds the targets that already hit a transport timeout during
// one request. Deps resolve concurrently on a shared ctx, hence the mutex.
type targetMemory struct {
	mu       sync.Mutex
	timedOut map[memTarget]struct{}
}

// WithTargetMemory attaches a fresh per-request memory: once a GET to a target
// times out, later GETs to that target in the same request make a single
// attempt (retries 0). Without it the runner retries as configured.
//
// The memory lives in ctx, never on Runner: the Runner is a process-wide
// singleton, so state there would let one request's timeout cut the retries of
// every other request to that OLT.
func WithTargetMemory(ctx context.Context) context.Context {
	return context.WithValue(ctx, targetMemoryKey{}, &targetMemory{timedOut: make(map[memTarget]struct{})})
}

func memoryFrom(ctx context.Context) *targetMemory {
	m, _ := ctx.Value(targetMemoryKey{}).(*targetMemory)
	return m
}

func (m *targetMemory) hasTimedOut(t Target) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.timedOut[memTarget{t.IP, t.Community}]
	return ok
}

func (m *targetMemory) markTimedOut(t Target) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.timedOut[memTarget{t.IP, t.Community}] = struct{}{}
}
