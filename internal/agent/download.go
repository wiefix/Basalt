package agent

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/wiefix/basalt/internal/buildinfo"
)

// Retries of a download the server turned away for now (wordpress.org
// answers 429 to busy addresses): the pause doubles from downloadPause,
// or is the server's Retry-After, at most downloadMaxPause.
var (
	downloadAttempts = 4
	downloadPause    = 2 * time.Second
	downloadMaxPause = 30 * time.Second
)

// downloadGet GETs url with Basalt's User-Agent, trying again after a
// pause while the answer is 429 or a temporary 5xx. The caller closes the
// body of the 200 it gets; any other status is an error.
func downloadGet(ctx context.Context, hc *http.Client, url string) (*http.Response, error) {
	pause := downloadPause
	for attempt := 1; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", "Basalt/"+buildinfo.Version+" (+https://github.com/wiefix/Basalt)")
		resp, err := hc.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusOK {
			return resp, nil
		}
		io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		err = fmt.Errorf("%s: %s", url, resp.Status)
		switch resp.StatusCode {
		case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		default:
			return nil, err
		}
		if attempt == downloadAttempts {
			return nil, err
		}
		wait := pause
		if s, perr := strconv.Atoi(resp.Header.Get("Retry-After")); perr == nil && s >= 0 {
			wait = time.Duration(s) * time.Second
		}
		wait = min(wait, downloadMaxPause)
		pause *= 2
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, fmt.Errorf("%w (%v)", err, ctx.Err())
		case <-t.C:
		}
	}
}
