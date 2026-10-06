package jobqueue

import "time"

// SetPruneSchedule overrides the delay before the first prune and the interval after it.
func SetPruneSchedule(w *Worker, first, every time.Duration) {
	w.firstPrune = first
	w.pruneEvery = every
}
