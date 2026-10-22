package pipeline

import (
	"context"
	"time"

	"github.com/tony-montemuro/phonetic/internal/store"
)

// StubStages returns placeholder implementations for every stage. Each waits
// delay (honoring cancellation) and succeeds. Later phases replace them with
// real stages one by one.
func StubStages(delay time.Duration) map[store.Status]Stage {
	stub := StageFunc(func(ctx context.Context, _ store.Article) error {
		if delay <= 0 {
			return nil
		}
		t := time.NewTimer(delay)
		defer t.Stop()
		select {
		case <-t.C:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	stages := make(map[store.Status]Stage)
	for _, s := range store.Stages() {
		stages[s] = stub
	}
	return stages
}
