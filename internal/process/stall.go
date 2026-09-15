package process

import (
	"sync"
	"time"
)

// stallPollsPerTimeout is how many times the watchdog samples progress within
// one stall timeout. Sampling at the timeout itself would let a tree that
// stopped just after a sample survive for nearly twice the configured window;
// four samples bound that overshoot to a quarter of it while keeping the poll
// cost -- two atomic loads and one already-sampled CPU figure -- negligible.
const stallPollsPerTimeout = 4

// minStallPoll floors the poll interval so a very short stall timeout, which
// only a test sets, cannot turn the watchdog into a spin loop.
const minStallPoll = 10 * time.Millisecond

// stallWatchdog terminates a run that makes no observable progress.
//
// It exists because Section 20.2's wall-clock timeouts cannot distinguish a
// large analysis unit from a wedged one: on a monorepo the legitimate run is
// the long one. Progress, not elapsed time, is what separates them. The
// signals are the raw bytes each output pipe has read -- counted before any
// output limit, and whether or not the bytes are kept, so a stream pointed at
// io.Discard still speaks -- and, where the platform samples a running tree,
// the tree's consumed CPU time, so a silent computation is not mistaken for a
// hang.
//
// A nil watchdog is the disabled form: stalledC returns a nil channel, which
// blocks forever in a select, and stopWatching does nothing. That is how a
// zero StallTimeout is expressed, so no caller needs a branch.
//
// Its goroutine is joined by stopWatching before the run returns, like the
// tree sampler's: no goroutine this package starts outlives its run.
type stallWatchdog struct {
	stop    chan struct{}
	done    chan struct{}
	stalled chan struct{}
	once    sync.Once
}

// startStallWatchdog begins watching the two pipes and the tree sampler. A
// timeout of zero, which is the default, returns nil: no detector.
func startStallWatchdog(timeout time.Duration, outPipe, errPipe *streamPipe, sampler *treeSampler) *stallWatchdog {
	if timeout <= 0 {
		return nil
	}
	w := &stallWatchdog{stop: make(chan struct{}), done: make(chan struct{}), stalled: make(chan struct{})}
	poll := max(timeout/stallPollsPerTimeout, minStallPoll)
	go func() {
		defer close(w.done)
		ticker := time.NewTicker(poll)
		defer ticker.Stop()
		last := stallProgress(outPipe, errPipe, sampler)
		// The clock is only ever read here and compared against itself, so a
		// wall-clock step does not make an active tree look wedged for longer
		// than one step.
		lastChange := time.Now()
		for {
			select {
			case <-w.stop:
				return
			case <-ticker.C:
			}
			now := stallProgress(outPipe, errPipe, sampler)
			if now != last {
				last, lastChange = now, time.Now()
				continue
			}
			if time.Since(lastChange) >= timeout {
				close(w.stalled)
				return
			}
		}
	}()
	return w
}

// stallProgress is the one number the watchdog watches. Summing the signals is
// sound because each is monotonic: any advance changes the sum, and only a tree
// where none of them advanced leaves it unchanged.
func stallProgress(outPipe, errPipe *streamPipe, sampler *treeSampler) int64 {
	return outPipe.progressed() + errPipe.progressed() + sampler.cpuTicks()
}

// stalledC is closed when the tree has made no progress for the whole timeout.
// On a nil watchdog it is a nil channel, which never fires.
func (w *stallWatchdog) stalledC() <-chan struct{} {
	if w == nil {
		return nil
	}
	return w.stalled
}

// stopWatching ends the watch and joins the goroutine. It is idempotent.
func (w *stallWatchdog) stopWatching() {
	if w == nil {
		return
	}
	w.once.Do(func() { close(w.stop) })
	<-w.done
}
