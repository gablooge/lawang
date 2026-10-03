package worker

import "context"

// Once is one turn of a drain goroutine: claim a batch and work it to the end. It is exported
// for the tests, which drive the drain a step at a time instead of starting the pool and
// waiting for it to notice things.
//
// Run is tested too, because the shutdown is only visible there.
func (d *Drain) Once(ctx context.Context) (int, error) { return d.once(ctx) }
