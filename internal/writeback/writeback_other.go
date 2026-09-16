//go:build !linux

package writeback

// run does nothing where the kernel offers no range writeback; the caller's
// fsync is the only writeback it requests.
func (p *Pacer) run([]string) {
	defer close(p.done)
	<-p.stop
}
