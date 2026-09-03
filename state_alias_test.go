package maestro

import (
	"sync"
	"testing"
)

// NewState must not alias the caller's map. Context is mutable execution state,
// so aliasing made one inputs map shared across concurrent runs — an
// unrecoverable "concurrent map read and map write".
func TestNewState_ClonesInputs(t *testing.T) {
	inputs := map[string]any{"deviceType": 8, "dslamIp": "10.85.1.1"}
	st := NewState(inputs)

	st.Context["injected"] = true
	if _, leaked := inputs["injected"]; leaked {
		t.Fatal("NewState aliased the caller's map: a Context write reached the input")
	}
	if got := st.Context["deviceType"]; got != 8 {
		t.Fatalf("clone lost an input value: deviceType = %v", got)
	}
}

// The shape that actually crashed: one inputs map, many states, run in parallel.
// Fails under -race (and fatals outright) if NewState aliases.
func TestNewState_SafeForConcurrentRunsOnOneInputsMap(t *testing.T) {
	inputs := map[string]any{"deviceType": 8, "dslamIp": "10.85.1.1"}

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			st := NewState(inputs)
			st.Context["result"] = i     // what SET / DATA_TRANSFORM do
			_ = st.Context["deviceType"] // what chunkScope does
		}(i)
	}
	wg.Wait()
}

func TestNewState_NilInputs(t *testing.T) {
	st := NewState(nil)
	if st.Context == nil {
		t.Fatal("NewState(nil) must still give a usable Context")
	}
	st.Context["k"] = 1
}
