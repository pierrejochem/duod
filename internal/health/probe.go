// Package health implements the analysis steps: wait until a slot is
// consistently healthy, or watch a URL for a fixed duration.
package health

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/pierrejochem/duod/internal/proxy"
)

// Probe performs HTTP checks.
type Probe struct {
	Client       *http.Client
	ExpectStatus int
	// RevisionHeader, if set, must be returned by the app and start with the
	// expected Git revision. This proves the answer comes from the new
	// containers and not from something else.
	RevisionHeader string
}

// NewProbe builds a probe with its own timeout and no redirects followed.
func NewProbe(timeout time.Duration, expect int) *Probe {
	return &Probe{
		Client: &http.Client{
			Timeout: timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		ExpectStatus: expect,
	}
}

// Get performs one request and returns status and response headers.
func Get(ctx context.Context, c *http.Client, t proxy.Target) (int, http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.URL, nil)
	if err != nil {
		return 0, nil, err
	}
	for k, vs := range t.Header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	req.Header.Set("User-Agent", "duod-health/1")
	resp, err := c.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	return resp.StatusCode, resp.Header, nil
}

// Check returns nil when the target answers with the expected status (and,
// when configured and wantRev is not empty, the expected revision header).
func (p *Probe) Check(ctx context.Context, t proxy.Target, wantRev string) error {
	code, hdr, err := Get(ctx, p.Client, t)
	if err != nil {
		return fmt.Errorf("%s: %w", t.Name, err)
	}
	if code != p.ExpectStatus {
		return fmt.Errorf("%s: status %d, want %d", t.Name, code, p.ExpectStatus)
	}
	if p.RevisionHeader != "" && wantRev != "" {
		got := hdr.Get(p.RevisionHeader)
		if len(got) < 7 || !strings.HasPrefix(wantRev, got) {
			return fmt.Errorf("%s: %s is %q, want revision %s", t.Name, p.RevisionHeader, got, wantRev)
		}
	}
	return nil
}

// CheckAll checks every target; the first failure wins.
func (p *Probe) CheckAll(ctx context.Context, ts []proxy.Target, wantRev string) error {
	for _, t := range ts {
		if err := p.Check(ctx, t, wantRev); err != nil {
			return err
		}
	}
	return nil
}

// WaitHealthy runs check until `successes` consecutive passes or the
// deadline expires (pre-promotion analysis).
func WaitHealthy(ctx context.Context, check func(context.Context) error, interval time.Duration, successes int, deadline time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	t := time.NewTicker(interval)
	defer t.Stop()
	streak := 0
	var lastErr error
	for {
		if err := check(ctx); err != nil {
			streak = 0
			if ctx.Err() == nil || lastErr == nil { // keep the real cause, not the deadline
				lastErr = err
			}
		} else {
			streak++
			if streak >= successes {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			if lastErr == nil {
				lastErr = ctx.Err()
			}
			return fmt.Errorf("not healthy after %s (%d/%d consecutive successes): %w", deadline, streak, successes, lastErr)
		case <-t.C:
		}
	}
}

// Watch checks url for the whole duration and fails as soon as
// `failures` consecutive checks fail (post-promotion analysis).
func (p *Probe) Watch(ctx context.Context, url string, interval, duration time.Duration, failures int) error {
	end := time.Now().Add(duration)
	t := time.NewTicker(interval)
	defer t.Stop()
	streak := 0
	target := proxy.Target{Name: url, URL: url}
	for {
		if err := p.Check(ctx, target, ""); err != nil {
			streak++
			if streak >= failures {
				return fmt.Errorf("%d consecutive failures: %w", streak, err)
			}
		} else {
			streak = 0
		}
		if time.Now().After(end) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}
