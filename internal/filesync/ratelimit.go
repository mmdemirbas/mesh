package filesync

import (
	"context"
	"io"

	"golang.org/x/time/rate"
)

// rateLimitedReader wraps an io.Reader with a token-bucket rate limiter.
// Each Read call waits until enough tokens are available.
type rateLimitedReader struct {
	r       io.Reader
	limiter *rate.Limiter
	ctx     context.Context
}

// newRateLimitedReader wraps r with the given limiter. If limiter is nil,
// returns r unchanged.
func newRateLimitedReader(ctx context.Context, r io.Reader, limiter *rate.Limiter) io.Reader {
	if limiter == nil {
		return r
	}
	return &rateLimitedReader{r: r, limiter: limiter, ctx: ctx}
}

func (r *rateLimitedReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if n > 0 {
		if waitErr := r.limiter.WaitN(r.ctx, n); waitErr != nil {
			return n, waitErr
		}
	}
	return n, err
}

// rateLimitedWriter wraps an io.Writer with a token-bucket rate limiter.
type rateLimitedWriter struct {
	w       io.Writer
	limiter *rate.Limiter
	ctx     context.Context
}

// newRateLimitedWriter wraps w with the given limiter. If limiter is nil,
// returns w unchanged.
func newRateLimitedWriter(ctx context.Context, w io.Writer, limiter *rate.Limiter) io.Writer {
	if limiter == nil {
		return w
	}
	return &rateLimitedWriter{w: w, limiter: limiter, ctx: ctx}
}

func (w *rateLimitedWriter) Write(p []byte) (int, error) {
	if err := w.limiter.WaitN(w.ctx, len(p)); err != nil {
		return 0, err
	}
	return w.w.Write(p)
}
