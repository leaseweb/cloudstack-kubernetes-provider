/*
 * Licensed to the Apache Software Foundation (ASF) under one
 * or more contributor license agreements.  See the NOTICE file
 * distributed with this work for additional information
 * regarding copyright ownership.  The ASF licenses this file
 * to you under the Apache License, Version 2.0 (the
 * "License"); you may not use this file except in compliance
 * with the License.  You may obtain a copy of the License at
 *
 *   http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied.  See the License for the
 * specific language governing permissions and limitations
 * under the License.
 */

package cloudstack

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/apache/cloudstack-go/v2/cloudstack"
	"k8s.io/client-go/util/flowcontrol"
)

// newTestTransport returns a transport without a rate limit, whose backoff records the sleeps instead of sleeping.
func newTestTransport(limiter flowcontrol.RateLimiter) (*rateLimitedTransport, *[]time.Duration) {
	var mu sync.Mutex
	var sleeps []time.Duration
	backoff := newThrottleBackoff()
	backoff.sleep = func(ctx context.Context, d time.Duration) error {
		mu.Lock()
		defer mu.Unlock()
		sleeps = append(sleeps, d)

		return ctx.Err()
	}

	return &rateLimitedTransport{
		next:           http.DefaultTransport,
		limiter:        limiter,
		backoff:        backoff,
		maxRetries:     throttleMaxRetries,
		attemptTimeout: apiRequestTimeout,
	}, &sleeps
}

// throttlingServer returns HTTP 429 for the first n requests and HTTP 200 after that. It records the request bodies.
func throttlingServer(t *testing.T, n int32) (*httptest.Server, *atomic.Int32, *[]string) {
	t.Helper()

	var requests atomic.Int32
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()

		if requests.Add(1) <= n {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"errorresponse":{"errorcode":429,"errortext":"too many requests"}}`))

			return
		}
		_, _ = w.Write([]byte(`{"ok":{}}`))
	}))
	t.Cleanup(srv.Close)

	return srv, &requests, &bodies
}

func doGet(t *testing.T, client *http.Client, u string) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, u, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	return client.Do(req) //nolint:wrapcheck // Test helper.
}

func TestRateLimitedTransport(t *testing.T) {
	t.Run("retries after HTTP 429 and returns the successful response", func(t *testing.T) {
		srv, requests, _ := throttlingServer(t, 2)
		tr, sleeps := newTestTransport(nil)

		resp, err := doGet(t, &http.Client{Transport: tr}, srv.URL)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		_ = resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("status = %d, want 200", resp.StatusCode)
		}
		if n := requests.Load(); n != 3 {
			t.Errorf("requests = %d, want 3", n)
		}
		if len(*sleeps) != 2 {
			t.Fatalf("sleeps = %v, want 2 backoff sleeps", *sleeps)
		}
		if (*sleeps)[0] < throttleInitialDelay || (*sleeps)[1] < 2*throttleInitialDelay {
			t.Errorf("sleeps = %v, want increasing delays from %v", *sleeps, throttleInitialDelay)
		}
	})

	t.Run("returns HTTP 429 after the maximum number of retries", func(t *testing.T) {
		srv, requests, _ := throttlingServer(t, 100)
		tr, _ := newTestTransport(nil)

		resp, err := doGet(t, &http.Client{Transport: tr}, srv.URL)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		if resp.StatusCode != http.StatusTooManyRequests {
			t.Errorf("status = %d, want 429", resp.StatusCode)
		}
		if !strings.Contains(string(body), "too many requests") {
			t.Errorf("body = %q, want the CloudStack error", body)
		}
		if n := requests.Load(); n != throttleMaxRetries+1 {
			t.Errorf("requests = %d, want %d", n, throttleMaxRetries+1)
		}
	})

	t.Run("retry sends the same POST body", func(t *testing.T) {
		srv, _, bodies := throttlingServer(t, 1)
		tr, _ := newTestTransport(nil)

		req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL, strings.NewReader(url.Values{"command": {"deployVirtualMachine"}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		resp, err := (&http.Client{Transport: tr}).Do(req)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		_ = resp.Body.Close()

		if len(*bodies) != 2 || (*bodies)[0] != "command=deployVirtualMachine" || (*bodies)[1] != (*bodies)[0] {
			t.Errorf("bodies = %q, want the same body twice", *bodies)
		}
	})

	t.Run("successful requests are not delayed and reset the backoff", func(t *testing.T) {
		srv, _, _ := throttlingServer(t, 0)
		tr, sleeps := newTestTransport(nil)

		for range 3 {
			resp, err := doGet(t, &http.Client{Transport: tr}, srv.URL)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			_ = resp.Body.Close()
		}
		if len(*sleeps) != 0 {
			t.Errorf("sleeps = %v, want none", *sleeps)
		}
	})

	t.Run("rate limit spaces out requests", func(t *testing.T) {
		srv, _, _ := throttlingServer(t, 0)
		tr, _ := newTestTransport(flowcontrol.NewTokenBucketRateLimiter(10, 1))

		begin := time.Now()
		for range 4 {
			resp, err := doGet(t, &http.Client{Transport: tr}, srv.URL)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			_ = resp.Body.Close()
		}
		// 4 requests at 10 QPS with burst 1: the last 3 wait about 100ms each.
		if elapsed := time.Since(begin); elapsed < 250*time.Millisecond {
			t.Errorf("4 requests took %v, want at least 250ms", elapsed)
		}
	})

	t.Run("canceled context stops the wait for the rate limit", func(t *testing.T) {
		srv, requests, _ := throttlingServer(t, 0)
		tr, _ := newTestTransport(flowcontrol.NewTokenBucketRateLimiter(0.001, 1))
		client := &http.Client{Transport: tr}

		resp, err := doGet(t, client, srv.URL)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		_ = resp.Body.Close()

		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
		resp, err = client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			t.Fatal("expected an error")
		}
		if n := requests.Load(); n != 1 {
			t.Errorf("requests = %d, want 1", n)
		}
	})
}

func TestThrottleBackoff(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	newBackoff := func() *throttleBackoff {
		b := newThrottleBackoff()
		b.now = func() time.Time { return now }

		return b
	}

	t.Run("delay doubles up to the maximum and includes jitter", func(t *testing.T) {
		b := newBackoff()
		want := throttleInitialDelay
		for range 10 {
			d := b.throttled(0)
			if d < want || d > want+want/2 {
				t.Fatalf("delay = %v, want between %v and %v", d, want, want+want/2)
			}
			want = min(2*want, throttleMaxDelay)
		}
	})

	t.Run("Retry-After is the minimum delay", func(t *testing.T) {
		b := newBackoff()
		if d := b.throttled(10 * time.Second); d < 10*time.Second {
			t.Errorf("delay = %v, want at least 10s", d)
		}
	})

	t.Run("Retry-After is limited to the maximum delay", func(t *testing.T) {
		b := newBackoff()
		if d := b.throttled(time.Hour); d > throttleMaxDelay {
			t.Errorf("delay = %v, want at most %v", d, throttleMaxDelay)
		}
	})

	t.Run("wait sleeps again when the backoff is extended", func(t *testing.T) {
		start := now
		defer func() { now = start }()

		b := newBackoff()
		var sleeps []time.Duration
		var extended time.Duration
		b.sleep = func(_ context.Context, d time.Duration) error {
			sleeps = append(sleeps, d)
			now = now.Add(d)
			if len(sleeps) == 1 {
				// Another request is throttled at the end of this sleep.
				extended = b.throttled(0)
			}

			return nil
		}

		first := b.throttled(0)
		if err := b.wait(t.Context()); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(sleeps) != 2 || sleeps[0] != first || sleeps[1] != extended {
			t.Errorf("sleeps = %v, want [%v %v]", sleeps, first, extended)
		}
	})

	t.Run("all requests wait during the backoff", func(t *testing.T) {
		b := newBackoff()
		var waited time.Duration
		b.sleep = func(_ context.Context, d time.Duration) error {
			waited = d

			return nil
		}

		d := b.throttled(0)
		if err := b.wait(t.Context()); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if waited != d {
			t.Errorf("waited %v, want %v", waited, d)
		}
	})

	t.Run("successful requests decrease the delay to zero", func(t *testing.T) {
		b := newBackoff()
		for range 3 {
			b.throttled(0)
		}
		for range 3 {
			b.succeeded()
		}
		if b.delay != 0 {
			t.Errorf("delay = %v, want 0", b.delay)
		}
	})
}

// TestNewHTTPClientWithCloudStackClient checks that the cloudstack-go client works with the HTTP client,
// and that a throttled API call is retried.
func TestNewHTTPClientWithCloudStackClient(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("command") != "listZones" {
			t.Errorf("command = %q, want listZones", r.URL.Query().Get("command"))
		}
		if requests.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"listzonesresponse":{"errorcode":429,"cserrorcode":9999,"errortext":"too many requests"}}`))

			return
		}
		_, _ = w.Write([]byte(`{"listzonesresponse":{"count":1,"zone":[{"id":"zone-1","name":"zone1"}]}}`))
	}))
	t.Cleanup(srv.Close)

	httpClient := newHTTPClient(false, 100, 10)
	// Do not wait for the backoff in the test.
	tr, ok := httpClient.Transport.(*rateLimitedTransport)
	if !ok {
		t.Fatalf("transport = %T, want *rateLimitedTransport", httpClient.Transport)
	}
	tr.backoff.sleep = func(context.Context, time.Duration) error { return nil }

	cs := cloudstack.NewAsyncClient(srv.URL, "key", "secret", true, cloudstack.WithHTTPClient(httpClient))
	r, err := cs.Zone.ListZones(cs.Zone.NewListZonesParams())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.Count != 1 || r.Zones[0].Id != "zone-1" {
		t.Errorf("zones = %+v, want zone-1", r.Zones)
	}
	if n := requests.Load(); n != 2 {
		t.Errorf("requests = %d, want 2", n)
	}
}

func TestNewCSCloudLimits(t *testing.T) {
	base := func() *CSConfig {
		cfg := &CSConfig{}
		cfg.Global.APIURL = "https://cloudstack.example/client/api"
		cfg.Global.APIKey = "key"
		cfg.Global.SecretKey = "secret"

		return cfg
	}
	ptr := func(v int) *int { return &v }

	t.Run("defaults", func(t *testing.T) {
		cs, err := newCSCloud(base())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cs.vmCache == nil || cs.vmCache.ttl != defaultVMCacheTTL {
			t.Errorf("vmCache = %+v, want TTL %v", cs.vmCache, defaultVMCacheTTL)
		}
	})

	t.Run("vm-cache-ttl 0 disables the cache", func(t *testing.T) {
		cfg := base()
		cfg.Global.VMCacheTTL = ptr(0)
		cs, err := newCSCloud(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cs.vmCache != nil {
			t.Errorf("vmCache = %+v, want nil", cs.vmCache)
		}
	})

	t.Run("negative vm-cache-ttl is an error", func(t *testing.T) {
		cfg := base()
		cfg.Global.VMCacheTTL = ptr(-1)
		if _, err := newCSCloud(cfg); err == nil {
			t.Error("expected an error")
		}
	})

	t.Run("burst below 1 with a rate limit is an error", func(t *testing.T) {
		cfg := base()
		cfg.Global.APIRateLimitBurst = ptr(0)
		if _, err := newCSCloud(cfg); err == nil {
			t.Error("expected an error")
		}
	})

	t.Run("burst is not checked when the rate limit is disabled", func(t *testing.T) {
		cfg := base()
		qps := 0.0
		cfg.Global.APIRateLimitQPS = &qps
		cfg.Global.APIRateLimitBurst = ptr(0)
		if _, err := newCSCloud(cfg); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})
}

func TestReadConfigLimits(t *testing.T) {
	cfg, err := readConfig(strings.NewReader(`
[Global]
api-url              = https://cloudstack.url
api-key              = a-valid-api-key
secret-key           = a-valid-secret-key
api-rate-limit-qps   = 2.5
api-rate-limit-burst = 5
vm-cache-ttl         = 0
`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Global.APIRateLimitQPS == nil || *cfg.Global.APIRateLimitQPS != 2.5 {
		t.Errorf("api-rate-limit-qps = %v, want 2.5", cfg.Global.APIRateLimitQPS)
	}
	if cfg.Global.APIRateLimitBurst == nil || *cfg.Global.APIRateLimitBurst != 5 {
		t.Errorf("api-rate-limit-burst = %v, want 5", cfg.Global.APIRateLimitBurst)
	}
	if cfg.Global.VMCacheTTL == nil || *cfg.Global.VMCacheTTL != 0 {
		t.Errorf("vm-cache-ttl = %v, want 0", cfg.Global.VMCacheTTL)
	}

	cfg, err = readConfig(strings.NewReader("[Global]\napi-url = https://cloudstack.url\n"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Global.APIRateLimitQPS != nil || cfg.Global.APIRateLimitBurst != nil || cfg.Global.VMCacheTTL != nil {
		t.Errorf("unset keys must be nil, got %+v", cfg.Global)
	}
}

func TestThrottleBackoffEpoch(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	newBackoff := func() *throttleBackoff {
		b := newThrottleBackoff()
		b.now = func() time.Time { return now }

		return b
	}

	t.Run("concurrent rejections increase the delay once", func(t *testing.T) {
		b := newBackoff()
		epoch := b.currentEpoch()
		first := b.throttledAt(epoch, 0)
		for range 5 {
			if d := b.throttledAt(epoch, 0); d != first {
				t.Errorf("delay = %v, want %v", d, first)
			}
		}
		if b.delay != throttleInitialDelay {
			t.Errorf("delay = %v, want %v", b.delay, throttleInitialDelay)
		}
	})

	t.Run("Retry-After of a concurrent rejection extends the backoff", func(t *testing.T) {
		b := newBackoff()
		epoch := b.currentEpoch()
		b.throttledAt(epoch, 0)
		if d := b.throttledAt(epoch, 10*time.Second); d != 10*time.Second {
			t.Errorf("delay = %v, want 10s", d)
		}
		if b.delay != throttleInitialDelay {
			t.Errorf("delay = %v, want %v", b.delay, throttleInitialDelay)
		}
	})

	t.Run("jitter does not exceed the maximum delay", func(t *testing.T) {
		b := newBackoff()
		for range 20 {
			if d := b.throttled(0); d > throttleMaxDelay {
				t.Fatalf("delay = %v, want at most %v", d, throttleMaxDelay)
			}
		}
	})
}

func TestIsAPIThrottled(t *testing.T) {
	tests := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{(&cloudstack.CSError{ErrorCode: http.StatusTooManyRequests, CSErrorCode: 9999, ErrorText: "too many requests"}).Error(), true},
		{fmt.Errorf("error retrieving list of hosts: %w", (&cloudstack.CSError{ErrorCode: http.StatusTooManyRequests}).Error()), true},
		{(&cloudstack.CSError{ErrorCode: 431, ErrorText: "unable to find VM"}).Error(), false},
		{errors.New("connection refused"), false},
	}
	for _, tt := range tests {
		if got := isAPIThrottled(tt.err); got != tt.want {
			t.Errorf("isAPIThrottled(%v) = %v, want %v", tt.err, got, tt.want)
		}
	}
}

// epochChangingLimiter is a rate limiter that calls onWait during the first wait.
type epochChangingLimiter struct {
	flowcontrol.RateLimiter
	waits  int
	onWait func()
}

func (l *epochChangingLimiter) Wait(context.Context) error {
	l.waits++
	if l.waits == 1 {
		l.onWait()
	}

	return nil
}

func TestWaitToSend(t *testing.T) {
	t.Run("waits for a backoff that starts during the wait for the rate limit", func(t *testing.T) {
		limiter := &epochChangingLimiter{}
		tr, sleeps := newTestTransport(limiter)
		var started time.Duration
		// Another request is throttled while this request waits for the rate limit.
		limiter.onWait = func() { started = tr.backoff.throttled(0) }

		epoch, err := tr.waitToSend(t.Context())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if limiter.waits != 2 {
			t.Errorf("rate limit waits = %d, want 2", limiter.waits)
		}
		if len(*sleeps) != 1 || (*sleeps)[0] > started {
			t.Errorf("sleeps = %v, want one backoff sleep of at most %v", *sleeps, started)
		}
		if epoch != tr.backoff.currentEpoch() {
			t.Errorf("epoch = %d, want %d", epoch, tr.backoff.currentEpoch())
		}
	})
}
