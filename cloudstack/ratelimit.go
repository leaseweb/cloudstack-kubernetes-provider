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
	"crypto/tls"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"k8s.io/client-go/util/flowcontrol"
	"k8s.io/klog/v2"
)

const (
	// defaultAPIRateLimitQPS is the default maximum number of CloudStack API requests per second.
	defaultAPIRateLimitQPS = 10.0

	// defaultAPIRateLimitBurst is the default number of CloudStack API requests that may exceed the QPS for a short time.
	defaultAPIRateLimitBurst = 20

	// apiRequestTimeout is the timeout for a single attempt of an API request.
	// This is the same as the default timeout of the cloudstack-go client.
	apiRequestTimeout = 60 * time.Second

	// throttleMaxRetries is the maximum number of retries of a request that CloudStack rejected with HTTP 429.
	throttleMaxRetries = 5

	// throttleInitialDelay and throttleMaxDelay bound the shared backoff after CloudStack rejected a request with HTTP 429.
	throttleInitialDelay = 1 * time.Second
	throttleMaxDelay     = 60 * time.Second
)

// newHTTPClient returns an HTTP client for the cloudstack-go client. It uses the same transport settings as
// cloudstack-go, and adds a client-side rate limit and a shared backoff when CloudStack API throttling
// (api.throttling.*) rejects requests with HTTP 429.
func newHTTPClient(sslNoVerify bool, qps float64, burst int) *http.Client {
	base := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: sslNoVerify}, //nolint:gosec // Explicitly configured with ssl-no-verify.
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	var limiter flowcontrol.RateLimiter
	if qps > 0 {
		limiter = flowcontrol.NewTokenBucketRateLimiter(float32(qps), burst)
	}

	return &http.Client{
		// The timeout is applied per attempt by the transport, so the time spent waiting
		// for the rate limiter or the backoff does not count towards it.
		Transport: &rateLimitedTransport{
			next:           base,
			limiter:        limiter,
			backoff:        newThrottleBackoff(),
			maxRetries:     throttleMaxRetries,
			attemptTimeout: apiRequestTimeout,
		},
	}
}

// rateLimitedTransport is an http.RoundTripper that limits the rate of requests and retries requests that
// CloudStack rejected with HTTP 429. CloudStack does not execute rejected requests, so a retry is safe.
type rateLimitedTransport struct {
	next           http.RoundTripper
	limiter        flowcontrol.RateLimiter // nil disables the rate limit
	backoff        *throttleBackoff
	maxRetries     int
	attemptTimeout time.Duration
}

// RoundTrip implements http.RoundTripper.
func (t *rateLimitedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		if err := t.backoff.wait(req.Context()); err != nil {
			return nil, fmt.Errorf("waiting for CloudStack API throttling backoff: %w", err)
		}

		if t.limiter != nil {
			if err := t.limiter.Wait(req.Context()); err != nil {
				return nil, fmt.Errorf("waiting for CloudStack API rate limit: %w", err)
			}
		}

		attemptReq, err := requestForAttempt(req, attempt)
		if err != nil {
			return nil, err
		}

		ctx, cancel := context.WithTimeout(req.Context(), t.attemptTimeout)
		resp, err := t.next.RoundTrip(attemptReq.WithContext(ctx))
		if err != nil {
			cancel()

			return nil, err //nolint:wrapcheck // A RoundTripper passes the errors of the next RoundTripper unchanged.
		}

		if resp.StatusCode != http.StatusTooManyRequests {
			t.backoff.succeeded()
			resp.Body = &cancelOnClose{ReadCloser: resp.Body, cancel: cancel}

			return resp, nil
		}

		// Always start the shared backoff, also when this request is not retried.
		delay := t.backoff.throttled(retryAfter(resp))
		if attempt >= t.maxRetries || (req.Body != nil && req.GetBody == nil) {
			klog.Warningf("CloudStack API throttled the request (HTTP 429), giving up after %d retries", attempt)
			resp.Body = &cancelOnClose{ReadCloser: resp.Body, cancel: cancel}

			return resp, nil
		}

		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		cancel()

		klog.V(2).Infof("CloudStack API throttled the request (HTTP 429), retry %d/%d in %v", attempt+1, t.maxRetries, delay)
	}
}

// requestForAttempt returns the request to send for the given attempt. The first attempt uses the original
// request. A retry uses a clone with a fresh body, because the body of the earlier attempt is consumed.
func requestForAttempt(req *http.Request, attempt int) (*http.Request, error) {
	if attempt == 0 {
		return req, nil
	}

	clone := req.Clone(req.Context())
	if req.Body != nil {
		body, err := req.GetBody()
		if err != nil {
			return nil, fmt.Errorf("resetting request body for retry: %w", err)
		}
		clone.Body = body
	}

	return clone, nil
}

// retryAfter returns the delay in the Retry-After header of the response, or 0 if there is none.
func retryAfter(resp *http.Response) time.Duration {
	if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 {
		return time.Duration(s) * time.Second
	}

	return 0
}

// cancelOnClose cancels the context of the attempt when the response body is closed.
type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()

	return err //nolint:wrapcheck // Close passes the error of the response body unchanged.
}

// throttleBackoff is a backoff that is shared by all requests. When CloudStack rejects a request with
// HTTP 429, all requests wait, not only the rejected one. The delay increases on each rejection and
// decreases on each successful request.
type throttleBackoff struct {
	mu    sync.Mutex
	delay time.Duration
	until time.Time
	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) error
}

func newThrottleBackoff() *throttleBackoff {
	return &throttleBackoff{
		now:   time.Now,
		sleep: sleepContext,
	}
}

// wait blocks until the shared backoff ends, or until ctx is done. When another request extends the
// backoff during the sleep, wait sleeps again until the new end.
func (b *throttleBackoff) wait(ctx context.Context) error {
	var waited time.Time
	for {
		b.mu.Lock()
		until := b.until
		d := until.Sub(b.now())
		b.mu.Unlock()

		if d <= 0 || until.Equal(waited) {
			return nil
		}

		if err := b.sleep(ctx, d); err != nil {
			return err
		}
		waited = until
	}
}

// throttled increases the delay, and starts a shared backoff of at least minDelay. minDelay is limited to
// throttleMaxDelay, so a large Retry-After does not stop all requests for a long time. It returns the length of the backoff.
func (b *throttleBackoff) throttled(minDelay time.Duration) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.delay == 0 {
		b.delay = throttleInitialDelay
	} else {
		b.delay = min(2*b.delay, throttleMaxDelay)
	}

	// Add up to 50% jitter, so the requests do not all start again at the same time.
	d := b.delay + rand.N(b.delay/2+1) //nolint:gosec // Jitter does not need a secure random number.
	d = max(d, min(minDelay, throttleMaxDelay))

	if until := b.now().Add(d); until.After(b.until) {
		b.until = until
	}

	return d
}

// succeeded decreases the delay after a request was not throttled.
func (b *throttleBackoff) succeeded() {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.delay == 0 {
		return
	}

	b.delay /= 2
	if b.delay < throttleInitialDelay {
		b.delay = 0
	}
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err() //nolint:wrapcheck // The caller wraps the error.
	case <-timer.C:
		return nil
	}
}
