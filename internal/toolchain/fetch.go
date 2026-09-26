package toolchain

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/paced"
)

// This file is the product's entire outbound network surface (Section 21). It
// fetches only a URL the embedded lock names, or that URL relocated under a
// configured mirror -- which keeps the original host as the mirror path's first
// segment -- and it verifies the declared size and digest before any caller may
// look at the bytes.

const (
	// maxRedirects is Section 11.7's "at most three redirects".
	maxRedirects = 3
	// fetchAttempts is the finite attempt budget of Section 22: a transport or
	// server fault, or a transfer the hang detector ended, is retried at once
	// up to this many attempts in all, and a digest mismatch is never retried.
	// There is no pause between attempts: a pause is a fixed wait, and a fault
	// that the next attempt cannot outlast is reported after the last one.
	fetchAttempts = 3
	// transferStall is the fetch path's hang-detector window: an attempt --
	// connection setup, the response, and the body -- is ended only once
	// nothing at all has arrived for this long.
	//
	// It is not a budget for the download. A payload is as large as a language
	// runtime and the link it arrives over is whatever the operator has, so no
	// wall clock can tell a slow transfer from a dead one: a deadline over the
	// fetch, or over any phase of it, is a rate requirement in disguise, and it
	// terminates the download that is working on the slower link. What
	// separates the two is whether anything arrives at all.
	//
	// It is a constant and not a configuration key because nothing about a
	// slow link or a large payload calls for a different value: those are
	// served by the byte signal, which never ends a transfer that is moving.
	// The window only says how long a host that has sent nothing is believed
	// to be working, and no operator setting makes that host's answer come.
	transferStall = 60 * time.Second
)

// assetDelegates names the hosts an asset host may hand a download body to. It
// is keyed by the host the lock names, never by the host actually dialed, so a
// mirror standing in front of that publisher forwards the same delegation.
// Section 11.7 allows redirects "to the same host set"; the release host the
// lock names answers an asset request with a 302 to a separate content host,
// so the set is the lock's host plus the hosts that host delegates to. A
// redirect anywhere else is refused; the digest check behind it is the real
// boundary, this keeps a hijacked redirect from becoming a request to an
// arbitrary host.
var assetDelegates = map[string][]string{
	"github.com": {"release-assets.githubusercontent.com", "objects.githubusercontent.com"},
}

// fetcher is the single net/http owner.
type fetcher struct {
	client   *http.Client
	mirror   *url.URL
	maxBytes int64
	// stall is the transfer hang detector's window; see transferStall.
	stall time.Duration
	log   *slog.Logger
}

func newFetcher(transport http.RoundTripper, mirror *url.URL, maxBytes int64, log *slog.Logger) *fetcher {
	if transport == nil {
		transport = &http.Transport{
			// Section 11.7 requires the standard proxy environment to be
			// honored: HTTP_PROXY, HTTPS_PROXY and NO_PROXY.
			//
			// No phase carries a clock of its own: connection setup, the
			// handshake and the wait for headers are all inside the attempt's
			// transfer watch, which ends them when nothing has moved.
			Proxy:               http.ProxyFromEnvironment,
			ForceAttemptHTTP2:   true,
			MaxIdleConnsPerHost: 2,
		}
	}
	return &fetcher{
		client: &http.Client{
			Transport:     transport,
			CheckRedirect: checkRedirect,
		},
		mirror:   mirror,
		maxBytes: maxBytes,
		stall:    transferStall,
		log:      log,
	}
}

// lockHostKey carries the lock URL's own host down the redirect chain. Under a
// mirror the first request's host is the mirror's, not the asset publisher's,
// so keying the delegate set off via[0] would refuse the very delegation a
// mirror proxying that publisher has to forward. The value is set on the
// request context, which every redirect request inherits.
type lockHostKey struct{}

func withLockHost(ctx context.Context, host string) context.Context {
	return context.WithValue(ctx, lockHostKey{}, host)
}

// checkRedirect enforces the redirect bound and the host set. via holds the
// requests already made, so following the Nth redirect arrives with len(via)
// == N and refusing above maxRedirects follows at most that many. The permitted
// set is the request's own origin host plus the delegates of the host the lock
// names, and it never grows as the chain does.
func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) > maxRedirects {
		return errors.New("too many redirects")
	}
	if req.URL.Scheme != "https" {
		return errors.New("redirect leaves https")
	}
	if req.URL.Hostname() == via[0].URL.Hostname() {
		return nil
	}
	lockHost, _ := via[0].Context().Value(lockHostKey{}).(string)
	for _, allowed := range assetDelegates[lockHost] {
		if req.URL.Hostname() == allowed {
			return nil
		}
	}
	return errors.New("redirect leaves the asset host set")
}

// target applies the mirror substitution of Section 11.7: the mirror replaces
// the scheme and host and keeps the original host as the first path segment, so
// one mirror serves every upstream and hosted asset without three origin trees
// colliding in a single namespace. It also reports the lock URL's own host,
// which is what the redirect policy keys its delegate set off.
func (f *fetcher) target(raw string) (*url.URL, string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, "", invalid("tool payload URL is unparsable")
	}
	if f.mirror == nil {
		return u, u.Hostname(), nil
	}
	mirrored := *f.mirror
	mirrored.Path = path.Join(f.mirror.Path, u.Host, u.Path)
	mirrored.RawQuery = ""
	return &mirrored, u.Hostname(), nil
}

// download streams one payload into dst while hashing it, and reports a typed
// failure unless the bytes are exactly the size and digest the lock pinned. The
// payload is never buffered: it goes to the staging file and the hash in one
// pass, so a several-hundred-megabyte runtime costs one 32-KiB copy buffer.
func (f *fetcher) download(ctx context.Context, name string, e Entry, p Payload, dst *os.File) error {
	if p.Size > f.maxBytes {
		return resourceLimit("tool %q payload is %d bytes, over the %d-byte tools.max_fetch_bytes cap", name, p.Size, f.maxBytes)
	}
	target, lockHost, err := f.target(p.URL)
	if err != nil {
		return err
	}
	ctx = withLockHost(ctx, lockHost)

	started := time.Now()
	var last error
	for attempt := 1; attempt <= fetchAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return model.Canceled(err)
		}
		if attempt > 1 {
			if err := rewind(dst); err != nil {
				return err
			}
		}
		err := f.attempt(ctx, name, target, p, dst)
		if err == nil {
			f.log.Info("managed tool payload fetched",
				"component", "toolchain", "tool", name, "version", e.Version,
				"digest", p.SHA256, "bytes", p.Size, "elapsed_ms", time.Since(started).Milliseconds())
			return nil
		}
		var typed *model.Error
		if errors.As(err, &typed) && !typed.Retryable {
			return err
		}
		last = err
	}
	return last
}

// attempt performs one request and one verified stream, under a hang detector
// on what arrives rather than a deadline on any part of it.
func (f *fetcher) attempt(ctx context.Context, name string, target *url.URL, p Payload, dst *os.File) error {
	// The request runs under a context this function can end on its own, which
	// is what aborts an attempt that has stopped moving. The caller's context
	// still ends it too, and the two are told apart below: the watch reports
	// whether it was the one that fired.
	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, target.String(), nil)
	if err != nil {
		return fetchFailed(name, "the payload request cannot be built", false)
	}
	// The watch starts before the request, so a host that accepts nothing, or
	// accepts the connection and never answers, is ended by the same window as
	// a body that stops: before the response nothing has arrived, which is no
	// progress. The body is attached once there is one.
	body := &progressReader{}
	watch := watchTransfer(body, f.stall, cancel)
	defer watch.stop()
	resp, err := f.client.Do(req)
	if err != nil {
		if watch.fired() {
			return fetchFailed(name, "the payload host delivered nothing for "+f.stall.String(), true)
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return model.Canceled(ctxErr)
		}
		return fetchFailed(name, "the payload host could not be reached", true)
	}
	defer resp.Body.Close()
	// The response itself is the host moving: its arrival restarts the quiet
	// window, so the time it took is not charged to the body's first byte.
	body.moved.Add(1)
	if resp.StatusCode != http.StatusOK {
		retryable := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
		return fetchFailed(name, "the payload host answered "+resp.Status, retryable)
	}
	body.r = resp.Body

	sum := sha256.New()
	// One byte past the declared size is read so a longer body is detected as a
	// disagreement with the lock instead of being silently truncated to it.
	n, err := io.Copy(io.MultiWriter(dst, sum), io.LimitReader(body, p.Size+1))
	if err != nil {
		if watch.fired() {
			return fetchFailed(name, "the payload stream delivered no bytes for "+f.stall.String(), true)
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return model.Canceled(ctxErr)
		}
		return fetchFailed(name, "the payload stream ended early", true)
	}
	if n != p.Size {
		return digestMismatch(name, "the payload is %d bytes where the lock pins %d", n, p.Size)
	}
	if got := hex.EncodeToString(sum.Sum(nil)); got != p.SHA256 {
		return digestMismatch(name, "the payload does not hash to the digest the lock pins")
	}
	if err := dst.Sync(); err != nil {
		return ioError("tool payload sync", err)
	}
	return nil
}

// progressReader counts every byte it hands on, which is the only honest
// progress signal a download has. Its reader is attached once the response
// exists; the watch reads only the counter, so it can run before then. The package already counts bytes this way
// where a payload is expanded (see the payload byte budget in extract.go); this
// is the same counting one layer earlier, on the wire.
type progressReader struct {
	r     io.Reader
	moved atomic.Int64
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		p.moved.Add(int64(n))
	}
	return n, err
}

// transferWatch ends an attempt that has stopped moving. It is the fetch
// path's hang detector: an attempt that received even one byte, or its
// response, inside the window is progressing however slow it is, and only one
// that received nothing has stopped.
type transferWatch struct {
	stalled atomic.Bool
	halt    chan struct{}
	done    chan struct{}
	once    sync.Once
}

// watchTransfer starts the detector over r and calls cancel once the transfer
// has delivered nothing for window. The caller must call stop on every path,
// which joins the goroutine: nothing this package starts outlives the attempt
// that started it.
func watchTransfer(r *progressReader, window time.Duration, cancel context.CancelFunc) *transferWatch {
	w := &transferWatch{halt: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(w.done)
		// Sampling at the window itself would let a transfer that died just
		// after a sample survive for nearly twice it; a quarter of the window
		// bounds that overshoot, as the subprocess stall watchdog does.
		ticker := time.NewTicker(window / 4)
		defer ticker.Stop()
		last, quietSince := r.moved.Load(), time.Now()
		for {
			select {
			case <-w.halt:
				return
			case now := <-ticker.C:
				if seen := r.moved.Load(); seen != last {
					last, quietSince = seen, now
					continue
				}
				if now.Sub(quietSince) >= window {
					w.stalled.Store(true)
					cancel()
					return
				}
			}
		}
	}()
	return w
}

// fired reports that this watch, and not the caller, ended the transfer. It is
// what keeps the failure honest: both end the copy by cancellation, and
// without it a wedged host would be reported as the caller cancelling.
func (w *transferWatch) fired() bool { return w.stalled.Load() }

// stop ends the watch and joins its goroutine. It is idempotent.
func (w *transferWatch) stop() {
	w.once.Do(func() { close(w.halt) })
	<-w.done
}

// rewind resets the staging file before a retry so a partial body from a failed
// attempt cannot be prefixed to the next one.
func rewind(dst *os.File) error {
	// At the pace: a tool payload is as large as the archive, and a retry
	// that freed all of it at once would hand the host that whole length.
	if err := paced.ShrinkFile(dst, 0); err != nil {
		return ioError("tool payload reset", err)
	}
	if _, err := dst.Seek(0, io.SeekStart); err != nil {
		return ioError("tool payload reset", err)
	}
	return nil
}

func fetchFailed(name, detail string, retryable bool) *model.Error {
	return (&model.Error{
		Code: model.CodeToolFetchFailed, Retryable: retryable,
		Message:     "the managed tool payload could not be fetched: " + detail,
		Remediation: "check network access, or set tools.mirror to a reachable host, or tools.offline with a pre-populated store",
	}).WithDetail("tool", name)
}

// digestMismatch is never retryable: bytes that disagree with the lock are not
// a transient fault, and Section 11.7 forbids retrying or executing them.
func digestMismatch(name, format string, args ...any) *model.Error {
	return toolError(model.CodeToolDigestMismatch, format, args...).
		WithDetail("tool", name).
		WithRemediation("the payload host is serving different bytes than this build pins; upgrade codectx or point tools.mirror at a correct mirror")
}
