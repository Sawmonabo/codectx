package process

import (
	"os"
	"sync"
	"time"
)

// stallPoll is the interval at which the watchdog samples progress within one
// stall timeout. It is exported to the runner as well as used here, because
// the tree sampler must refresh the CPU signal at least as often as this, or a
// silent but computing tree would look unchanged for a whole short window.
func stallPoll(timeout time.Duration) time.Duration {
	return max(timeout/stallPollsPerTimeout, minStallPoll)
}

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
// hang -- and the size of every file the caller named in Spec.ProgressFiles,
// which is the signal a tool that writes its answer straight to an output file
// and says nothing on either pipe has. That last signal is what keeps the
// detector honest where there is no CPU sampling: on a platform with no tree
// sampler cpuTicks is a constant zero, so without it a quiet writer would have
// had no signal at all and would have been killed mid-work.
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
func startStallWatchdog(timeout time.Duration, outPipe, errPipe *streamPipe, sampler *treeSampler,
	progressFiles []string) *stallWatchdog {

	if timeout <= 0 {
		return nil
	}
	w := &stallWatchdog{stop: make(chan struct{}), done: make(chan struct{}), stalled: make(chan struct{})}
	poll := stallPoll(timeout)
	go func() {
		defer close(w.done)
		ticker := time.NewTicker(poll)
		defer ticker.Stop()
		last := stallProgress(outPipe, errPipe, sampler, progressFiles)
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
			now := stallProgress(outPipe, errPipe, sampler, progressFiles)
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
// where none of them advanced leaves it unchanged. A signal the platform or the
// caller does not supply contributes a constant zero, which neither invents
// progress nor hides another signal's.
func stallProgress(outPipe, errPipe *streamPipe, sampler *treeSampler, progressFiles []string) int64 {
	return outPipe.progressed() + errPipe.progressed() + sampler.cpuTicks() + progressBytes(progressFiles)
}

// progressBytes sums the bytes the run has written to the outputs the caller
// named. A path that does not exist yet, or cannot be stated at this instant,
// contributes nothing: the tool creates its output part way through the run,
// and an unreadable path is the absence of a signal, not a reason to fail a
// run the pipes may still be speaking for.
//
// A named directory is summed one level deep over its regular entries, because
// a step whose output is a directory of files grows the files, not the
// directory inode. The sweep is not recursive: it is a liveness question asked
// four times per stall timeout, not a measurement.
//
// It is not monotonic in the strict sense -- a tool that rewrites its output
// can shrink it -- but the watchdog compares the sum for equality, not for
// growth, so any change at all is progress.
func progressBytes(paths []string) int64 {
	var total int64
	for _, path := range paths {
		if path == "" {
			continue
		}
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		if !info.IsDir() {
			total += info.Size()
			continue
		}
		entries, err := os.ReadDir(path)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			if sub, err := entry.Info(); err == nil {
				total += sub.Size()
			}
		}
	}
	return total
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
