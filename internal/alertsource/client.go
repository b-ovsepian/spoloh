package alertsource

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultBaseURL is the Ubilling aerial alerts endpoint.
const DefaultBaseURL = "https://ubilling.net.ua/aerialalerts/"

// DocsURL documents that endpoint, including every value its "?source="
// parameter accepts. It is what the dashboard links an operator to, because
// the question they have on that screen — what is "jaam", and why would I
// pick it over "skog" — is answered here and nowhere in this repository.
//
// Separate from DefaultBaseURL on purpose: one is fetched by the daemon
// every few seconds and the other is opened by a person once. Pointing a
// reader at the JSON endpoint would show them a payload, not an
// explanation.
const DocsURL = "https://wiki.ubilling.net.ua/doku.php?id=aerialalertsapi"

// maxBody caps how much of a response we read; the real payload is a few KB and
// an unbounded read on a broken upstream would be a memory hazard on a Pi 2.
const maxBody = 1 << 20

// Source produces alert snapshots.
type Source interface {
	Fetch(ctx context.Context) (Snapshot, error)
}

// RateLimitError reports an upstream 429. Callers must respect RetryAfter:
// several devices behind one NAT share the public IP and therefore the limit.
type RateLimitError struct {
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("rate limited, retry after %v", e.RetryAfter)
}

// Client fetches from a list of upstream sources, in order, until one answers.
type Client struct {
	baseURL string
	sources []string
	hc      *http.Client
}

func New(baseURL string, sources []string, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	if len(sources) == 0 {
		sources = []string{"default"}
	}
	return &Client{baseURL: baseURL, sources: sources, hc: hc}
}

// Fetch tries each configured source in order and returns the first success.
func (c *Client) Fetch(ctx context.Context) (Snapshot, error) {
	var errs []error
	for _, src := range c.sources {
		snap, err := c.fetchOne(ctx, src)
		if err == nil {
			return snap, nil
		}
		errs = append(errs, fmt.Errorf("source %s: %w", src, err))
		if ctx.Err() != nil {
			break
		}
	}
	return Snapshot{}, errors.Join(errs...)
}

func (c *Client) fetchOne(ctx context.Context, source string) (Snapshot, error) {
	url := c.baseURL
	if !strings.Contains(url, "?") {
		url += "?"
	} else {
		url += "&"
	}
	// The source name comes from the config file, where an operator can type
	// anything; an unescaped "&" would silently turn the rest of it into a
	// second query parameter. (net/url is imported under an alias because the
	// local variable below is called url.)
	url += "source=" + neturl.QueryEscape(source)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Snapshot{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "spoloh/1.0 (+https://github.com/b-ovsepian/spoloh)")

	resp, err := c.hc.Do(req)
	if err != nil {
		return Snapshot{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		return Snapshot{}, &RateLimitError{RetryAfter: retryAfter(resp)}
	}
	if resp.StatusCode != http.StatusOK {
		return Snapshot{}, fmt.Errorf("http %d", resp.StatusCode)
	}
	// A 200 carrying HTML is an upstream error page. Parsing it as state would
	// be worse than reporting a failure.
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "json") {
		return Snapshot{}, fmt.Errorf("unexpected content type %q", ct)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return Snapshot{}, err
	}
	return Parse(body)
}

func retryAfter(resp *http.Response) time.Duration {
	if v := resp.Header.Get("Retry-After"); v != "" {
		if secs, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && secs >= 0 {
			return time.Duration(secs) * time.Second
		}
	}
	return 15 * time.Second
}

// Backoff returns the delay before retry number attempt: 15s doubling to a
// 5 minute cap. The cascade never gives up, because a permanently silent
// announcer is the worst failure this device can have.
func Backoff(attempt int) time.Duration {
	const (
		base = 15 * time.Second
		cap_ = 5 * time.Minute
	)
	if attempt < 1 {
		attempt = 1
	}
	d := base
	for i := 1; i < attempt; i++ {
		d *= 2
		if d >= cap_ {
			return cap_
		}
	}
	if d > cap_ {
		return cap_
	}
	return d
}
