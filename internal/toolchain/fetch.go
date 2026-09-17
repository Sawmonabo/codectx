package toolchain

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net"
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
	// fetchAttempts is the fixed attempt budget of Section 22: a transport or
	// server fault is retried a bounded number of times inside the one deadline
	// the caller gave, and a digest mismatch is never retried at all.
	fetchAttempts = 3
	// retryBackoff is the pause between attempts. It is fixed rather than
	// exponential because the whole budget is already bounded by the deadline.
	retryBackoff = 2 * time.Second
	// dialTimeout and tlsTimeout bound connection setup so a black-holed host
	// cannot hold an attempt open before a byte moves.
	dialTimeout = 30 * time.Second
	tlsTimeout  = 30 * time.Second
	// transferStall is how long a started transfer may deliver no bytes at all
	// before it is treated as wedged.
	//
	// It is a hang detector and not a budget for the download. A payload is as
	// large as a language runtime and the link it arrives over is whatever the
	// operator has, so no wall clock can tell a slow transfer from a dead one:
	// a deadline over the whole fetch is a rate requirement in disguise, and it
	// terminates the download that is working on the slower link. What
	// separates the two is whether bytes arrive at all.
	//
	// It sits above the response-header timeout so a host that is answering
	// slowly is ended by that bound, which is about the answer, rather than by
	// this one, which is about the body.
	transferStall = 60 * time.Second
	// installWait is how long a resolution waits for another process's install
	// of the same tool before reporting it busy. It is a queueing bound and not
	// a deadline on the peer: the peer's own transfer watch decides whether it
	// is progressing, and this only decides how long this process is willing to
	// stand behind it rather than fail with a busy store.
	installWait = 10 * time.Minute
)

// assetDelegates names the hosts an asset host may hand a download body to. It
// is keyed by the host the lock names, never by the host actually dialed, so a
// mirror standing in front of that publisher forwards the same delegation.
// Section 11.7 allows redirects "to the same host set"; a GitHub release asset
// is answered by github.com with a 302 to a separate content host, so the set
// is the lock's host plus the hosts that host is known to delegate to. Observed
// 2026-09-13: github.com -> release-assets.githubusercontent.com. A redirect
// anywhere else is refused; the digest check behind it is the real boundary,
// this keeps a hijacked redirect from becoming a request to an arbitrary host.
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
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: dialTimeout}).DialContext,
			TLSHandshakeTimeout:   tlsTimeout,
			ResponseHeaderTimeout: tlsTimeout,
			ForceAttemptHTTP2:     true,
			MaxIdleConnsPerHost:   2,
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
// pass, so a several-hundred-megabyte JDK costs one 32-KiB copy buffer.
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
			select {
			case <-ctx.Done():
				return model.Canceled(ctx.Err())
			case <-time.After(retryBackoff):
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
// on the bytes received rather than a deadline on the whole of it.
func (f *fetcher) attempt(ctx context.Context, name string, target *url.URL, p Payload, dst *os.File) error {
	// The request runs under a context this function can end on its own, which
	// is what aborts a transfer that has stopped delivering. The caller's
	// context still ends it too, and the two are told apart below: the watch
	// reports whether it was the one that fired.
	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, target.String(), nil)
	if err != nil {
		return fetchFailed(name, "the payload request cannot be built", false)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return model.Canceled(ctxErr)
		}
		return fetchFailed(name, "the payload host could not be reached", true)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		retryable := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
		return fetchFailed(name, "the payload host answered "+resp.Status, retryable)
	}

	sum := sha256.New()
	body := &progressReader{r: resp.Body}
	watch := watchTransfer(body, f.stall, cancel)
	defer watch.stop()
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
// progress signal a download has. The package already counts bytes this way
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

// transferWatch ends a transfer that has stopped moving. It is the fetch
// path's hang detector: a transfer that delivered even one byte inside the
// window is progressing however slow it is, and only one that delivered none
// has stopped.
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
