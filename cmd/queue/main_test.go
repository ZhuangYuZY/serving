/*
Copyright 2019 The Knative Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"go.opencensus.io/plugin/ochttp"
	pkgnet "knative.dev/pkg/network"
	"knative.dev/pkg/ptr"
	"knative.dev/pkg/tracing"
	tracingconfig "knative.dev/pkg/tracing/config"
	tracetesting "knative.dev/pkg/tracing/testing"
	"knative.dev/serving/pkg/activator"
	"knative.dev/serving/pkg/network"
	"knative.dev/serving/pkg/queue"
	"knative.dev/serving/pkg/queue/health"

	. "knative.dev/pkg/logging/testing"
)

const wantHost = "a-better-host.com"

func TestHandlerReqEvent(t *testing.T) {
	var httpHandler http.HandlerFunc = func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(activator.RevisionHeaderName) != "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		if r.Header.Get(activator.RevisionHeaderNamespace) != "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		if got, want := r.Host, wantHost; got != want {
			t.Errorf("Host header = %q, want: %q", got, want)
		}
		if got, want := r.Header.Get(network.OriginalHostHeader), ""; got != want {
			t.Errorf("%s header was preserved", network.OriginalHostHeader)
		}

		w.WriteHeader(http.StatusOK)
	}

	server := httptest.NewServer(httpHandler)
	serverURL, _ := url.Parse(server.URL)

	defer server.Close()
	proxy := httputil.NewSingleHostReverseProxy(serverURL)

	params := queue.BreakerParams{QueueDepth: 10, MaxConcurrency: 10, InitialCapacity: 10}
	breaker := queue.NewBreaker(params)
	stats := network.NewRequestStats(time.Now())
	h := proxyHandler(breaker, stats, true /*tracingEnabled*/, proxy)

	writer := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://example.com", nil)

	// Verify the Original host header processing.
	req.Host = "nimporte.pas"
	req.Header.Set(network.OriginalHostHeader, wantHost)

	req.Header.Set(network.ProxyHeaderName, activator.Name)
	h(writer, req)

	if got := stats.Report(time.Now()).ProxiedRequestCount; got != 1 {
		t.Errorf("ProxiedRequestCount = %v, want 1", got)
	}
}

func TestProbeHandler(t *testing.T) {
	logger = TestLogger(t)
	f, err := ioutil.TempFile("", "labels")
	if err != nil {
		t.Errorf("Failed to created temporary file: %v", err)
	}
	defer os.RemoveAll(f.Name())
	if _, err = f.Write([]byte("true")); err != nil {
		t.Errorf("failed to write to the file %v", err)
	}
	f.Close()

	testcases := []struct {
		name          string
		prober        func() bool
		wantCode      int
		wantBody      string
		requestHeader string
		cfg           config
	}{{
		name:          "unexpected probe header",
		prober:        func() bool { return true },
		wantCode:      http.StatusBadRequest,
		wantBody:      fmt.Sprintf(badProbeTemplate, "test-probe"),
		requestHeader: "test-probe",
	}, {
		name:          "true probe function",
		prober:        func() bool { return true },
		wantCode:      http.StatusOK,
		wantBody:      queue.Name,
		requestHeader: queue.Name,
	}, {
		name:          "fail readiness",
		prober:        nil,
		wantCode:      http.StatusBadRequest,
		wantBody:      "failing healthcheck",
		requestHeader: queue.Name,
		cfg:           config{DownwardAPILabelsPath: f.Name()},
	}, {
		name:          "nil probe function",
		prober:        nil,
		wantCode:      http.StatusInternalServerError,
		wantBody:      "no probe",
		requestHeader: queue.Name,
	}, {
		name:          "false probe function",
		prober:        func() bool { return false },
		wantCode:      http.StatusServiceUnavailable,
		wantBody:      "queue not ready",
		requestHeader: queue.Name,
	}}

	healthState := &health.State{}
	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			writer := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "http://example.com", nil)
			req.Header.Set(network.ProbeHeaderName, tc.requestHeader)

			h := knativeProbeHandler(healthState, tc.prober, true /* isAggresive*/, true /*tracingEnabled*/, nil, tc.cfg, logger)
			h(writer, req)

			if got, want := writer.Code, tc.wantCode; got != want {
				t.Errorf("probe status = %v, want: %v", got, want)
			}
			if got, want := strings.TrimSpace(writer.Body.String()), tc.wantBody; got != want {
				// \r\n might be inserted, etc.
				t.Errorf("probe body = %q, want: %q, diff: %s", got, want, cmp.Diff(got, want))
			}
		})
	}
}

func TestProbeQueueInvalidPort(t *testing.T) {
	if err := probeQueueHealthPath(1, probeConfig{QueueServingPort: 0}); err == nil {
		t.Error("Expected error, got nil")
	} else if diff := cmp.Diff(err.Error(), "port must be a positive value, got 0"); diff != "" {
		t.Errorf("Unexpected not ready message: %s", diff)
	}
}

func TestProbeQueueConnectionFailure(t *testing.T) {
	if err := probeQueueHealthPath(1, probeConfig{QueueServingPort: 12345}); err == nil {
		t.Error("Expected error, got nil")
	}
}

func TestProbeQueueNotReady(t *testing.T) {
	queueProbed := ptr.Int32(0)
	ts := newProbeTestServer(func(w http.ResponseWriter) {
		atomic.AddInt32(queueProbed, 1)
		w.WriteHeader(http.StatusBadRequest)
	})

	defer ts.Close()

	u, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatalf("%s is not a valid URL: %v", ts.URL, err)
	}

	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("Failed to convert port(%s) to int: %v", u.Port(), err)
	}

	err = probeQueueHealthPath(1, probeConfig{QueueServingPort: port})

	if diff := cmp.Diff(err.Error(), "probe returned not ready"); diff != "" {
		t.Errorf("Unexpected not ready message: %s", diff)
	}

	if atomic.LoadInt32(queueProbed) == 0 {
		t.Error("Expected the queue proxy server to be probed")
	}
}

func TestProbeQueueReady(t *testing.T) {
	queueProbed := ptr.Int32(0)
	ts := newProbeTestServer(func(w http.ResponseWriter) {
		atomic.AddInt32(queueProbed, 1)
		w.WriteHeader(http.StatusOK)
	})

	defer ts.Close()

	u, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatalf("%s is not a valid URL: %v", ts.URL, err)
	}

	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("Failed to convert port(%s) to int: %v", u.Port(), err)
	}

	if err = probeQueueHealthPath(1, probeConfig{QueueServingPort: port}); err != nil {
		t.Errorf("probeQueueHealthPath(%d, 1s) = %s", port, err)
	}

	if atomic.LoadInt32(queueProbed) == 0 {
		t.Error("Expected the queue proxy server to be probed")
	}
}

func TestProbeFailFast(t *testing.T) {
	f, err := ioutil.TempFile("", "labels")
	if err != nil {
		t.Errorf("Failed to created temporary file: %v", err)
	}
	defer os.RemoveAll(f.Name())

	ts := newProbeTestServer(func(w http.ResponseWriter) {
		if preferScaledown, err := preferPodForScaledown(f.Name()); err != nil {
			t.Fatal("Failed to process downward API labels:", err)
		} else if preferScaledown {
			w.WriteHeader(http.StatusBadRequest)
		}
	})
	defer ts.Close()

	u, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatalf("%s is not a valid URL: %v", ts.URL, err)
	}

	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("Failed to convert port(%s) to int: %v", u.Port(), err)
	}

	if _, err = f.Write([]byte("true")); err != nil {
		t.Errorf("failed writing to file %v", err)
	}
	f.Close()

	start := time.Now()
	if err = probeQueueHealthPath(1 /*seconds*/, probeConfig{
		QueueServingPort:      port,
		DownwardAPILabelsPath: f.Name(),
	}); err == nil {
		t.Error("probeQueueHealthPath did not fail")
	}

	// if fails due to timeout and not cancelation, then it took too long
	if time.Since(start) >= 1*time.Second {
		t.Error("took too long to fail")
	}
}

func TestProbeQueueTimeout(t *testing.T) {
	queueProbed := ptr.Int32(0)
	ts := newProbeTestServer(func(w http.ResponseWriter) {
		atomic.AddInt32(queueProbed, 1)
		time.Sleep(2 * time.Second)
		w.WriteHeader(http.StatusOK)
	})

	defer ts.Close()

	u, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatalf("%s is not a valid URL: %v", ts.URL, err)
	}

	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("failed to convert port(%s) to int", u.Port())
	}

	timeout := 1
	if err = probeQueueHealthPath(timeout, probeConfig{QueueServingPort: port}); err == nil {
		t.Errorf("Expected probeQueueHealthPath(%d, %v) to return timeout error", port, timeout)
	}

	ts.Close()

	if atomic.LoadInt32(queueProbed) == 0 {
		t.Error("Expected the queue proxy server to be probed")
	}
}

func TestProbeQueueDelayedReady(t *testing.T) {
	count := ptr.Int32(0)
	ts := newProbeTestServer(func(w http.ResponseWriter) {
		if atomic.AddInt32(count, 1) < 9 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	defer ts.Close()

	u, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatalf("%s is not a valid URL: %v", ts.URL, err)
	}

	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("Failed to convert port(%s) to int: %v", u.Port(), err)
	}

	timeout := 0
	if err := probeQueueHealthPath(timeout, probeConfig{QueueServingPort: port}); err != nil {
		t.Errorf("probeQueueHealthPath(%d) = %s", port, err)
	}
}

func TestQueueTraceSpans(t *testing.T) {
	logger = TestLogger(t)
	testcases := []struct {
		name          string
		prober        func() bool
		wantSpans     int
		requestHeader string
		infiniteCC    bool
		probeWillFail bool
		probeTrace    bool
		enableTrace   bool
	}{{
		name:          "proxy trace",
		prober:        func() bool { return true },
		wantSpans:     3,
		requestHeader: "",
		probeWillFail: false,
		probeTrace:    false,
		enableTrace:   true,
	}, {
		name:          "proxy trace, no breaker",
		prober:        func() bool { return true },
		wantSpans:     2,
		requestHeader: "",
		probeWillFail: false,
		probeTrace:    false,
		enableTrace:   true,
		infiniteCC:    true,
	}, {
		name:          "true prober function with probe trace",
		prober:        func() bool { return true },
		wantSpans:     1,
		requestHeader: queue.Name,
		probeWillFail: false,
		probeTrace:    true,
		enableTrace:   true,
	}, {
		name:          "unexpected probe header",
		prober:        func() bool { return true },
		wantSpans:     1,
		requestHeader: "test-probe",
		probeWillFail: true,
		probeTrace:    true,
		enableTrace:   true,
	}, {
		name:          "nil prober function",
		prober:        nil,
		wantSpans:     1,
		requestHeader: queue.Name,
		probeWillFail: true,
		probeTrace:    true,
		enableTrace:   true,
	}, {
		name:          "false prober function",
		prober:        func() bool { return false },
		wantSpans:     1,
		requestHeader: queue.Name,
		probeWillFail: true,
		probeTrace:    true,
		enableTrace:   true,
	}, {
		name:          "no traces",
		prober:        func() bool { return true },
		wantSpans:     0,
		requestHeader: queue.Name,
		probeWillFail: false,
		probeTrace:    false,
		enableTrace:   false,
	}}

	healthState := &health.State{}
	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			// Create tracer with reporter recorder
			reporter, co := tracetesting.FakeZipkinExporter()
			defer reporter.Close()
			oct := tracing.NewOpenCensusTracer(co)
			defer oct.Finish()

			cfg := tracingconfig.Config{
				Backend: tracingconfig.Zipkin,
				Debug:   true,
			}
			if !tc.enableTrace {
				cfg.Backend = tracingconfig.None
			}
			if err := oct.ApplyConfig(&cfg); err != nil {
				t.Errorf("Failed to apply tracer config: %v", err)
			}

			writer := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "http://example.com", nil)

			if !tc.probeTrace {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(http.StatusOK)
				}))
				defer server.Close()
				serverURL, _ := url.Parse(server.URL)

				proxy := httputil.NewSingleHostReverseProxy(serverURL)
				params := queue.BreakerParams{QueueDepth: 10, MaxConcurrency: 10, InitialCapacity: 10}
				var breaker *queue.Breaker
				if !tc.infiniteCC {
					breaker = queue.NewBreaker(params)
				}
				proxy.Transport = &ochttp.Transport{
					Base: pkgnet.AutoTransport,
				}

				h := proxyHandler(breaker, network.NewRequestStats(time.Now()), true /*tracingEnabled*/, proxy)
				h(writer, req)
			} else {
				h := knativeProbeHandler(healthState, tc.prober, true /* isAggresive*/, true /*tracingEnabled*/, nil, config{}, logger)
				req.Header.Set(network.ProbeHeaderName, tc.requestHeader)
				h(writer, req)
			}

			gotSpans := reporter.Flush()
			if len(gotSpans) != tc.wantSpans {
				t.Errorf("Got %d spans, expected %d", len(gotSpans), tc.wantSpans)
			}
			spanNames := []string{"probe", "/", "queue_proxy"}
			if !tc.probeTrace {
				spanNames = spanNames[1:]
			}
			// We want to add `queueWait` span only if there is possible queueing
			// and if the tests actually expects tracing.
			if !tc.infiniteCC && tc.wantSpans > 1 {
				spanNames = append([]string{"queue_wait"}, spanNames...)
			}
			gs := []string{}
			for i := 0; i < len(gotSpans); i++ {
				gs = append(gs, gotSpans[i].Name)
			}
			t.Log(spanNames)
			t.Log(gs)
			for i, spanName := range spanNames[:tc.wantSpans] {
				if gotSpans[i].Name != spanName {
					t.Errorf("Span[%d].Name = %q, want: %q", i, gotSpans[i].Name, spanName)
				}
				if tc.probeWillFail {
					if len(gotSpans[i].Annotations) == 0 {
						t.Error("Expected error as value for failed span Annotation, got empty Annotation")
					} else if gotSpans[i].Annotations[0].Value != "error" {
						t.Errorf("Expected error as value for failed span Annotation, got %q", gotSpans[i].Annotations[0].Value)
					}
				}
			}
		})
	}
}

func BenchmarkProxyHandler(b *testing.B) {
	var baseHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	stats := network.NewRequestStats(time.Now())
	reportTicker := time.NewTicker(reportingPeriod)
	defer reportTicker.Stop()
	promStatReporter, err := queue.NewPrometheusStatsReporter(
		"ns", "testksvc", "testksvc",
		"pod", reportingPeriod)
	if err != nil {
		b.Fatal("Failed to create stats reporter:", err)
	}
	go func() {
		for now := range reportTicker.C {
			promStatReporter.Report(stats.Report(now))
		}
	}()
	req := httptest.NewRequest(http.MethodPost, "http://example.com", nil)
	req.Header.Set(network.OriginalHostHeader, wantHost)

	tests := []struct {
		label   string
		breaker *queue.Breaker
	}{{
		label:   "breaker-10",
		breaker: queue.NewBreaker(queue.BreakerParams{QueueDepth: 10, MaxConcurrency: 10, InitialCapacity: 10}),
	}, {
		label:   "breaker-infinite",
		breaker: nil,
	}}
	for _, tc := range tests {
		h := proxyHandler(tc.breaker, stats, true /*tracingEnabled*/, baseHandler)
		b.Run(fmt.Sprintf("sequential-%s", tc.label), func(b *testing.B) {
			resp := httptest.NewRecorder()
			for j := 0; j < b.N; j++ {
				h(resp, req)
			}
		})
		b.Run(fmt.Sprintf("parallel-%s", tc.label), func(b *testing.B) {
			b.RunParallel(func(pb *testing.PB) {
				resp := httptest.NewRecorder()
				for pb.Next() {
					h(resp, req)
				}
			})
		})
	}
}

func newProbeTestServer(f func(w http.ResponseWriter)) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(network.UserAgentKey) == network.QueueProxyUserAgent {
			f(w)
		}
	}))
}
