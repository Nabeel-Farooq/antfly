// Copyright 2025 Antfly, Inc.
//
// Licensed under the Elastic License 2.0 (ELv2); you may not use this file
// except in compliance with the Elastic License 2.0. You may obtain a copy of
// the Elastic License 2.0 at
//
//     https://www.antfly.io/licensing/ELv2-license
//
// Unless required by applicable law or agreed to in writing, software distributed
// under the Elastic License 2.0 is distributed on an "AS IS" BASIS, WITHOUT
// WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the
// Elastic License 2.0 for the specific language governing permissions and
// limitations.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"maps"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/antflydb/antfly/lib/types"
)

const (
	serverAddr           = ":8080"
	leaderUpdateInterval = 5 * time.Second
	healthCheckInterval  = 10 * time.Second
	idleCleanupInterval  = 5 * time.Minute
)

var healthClient = &http.Client{
	Timeout: 3 * time.Second,
}

type Backend struct {
	ID        types.ID
	URL       *url.URL
	Proxy     *httputil.ReverseProxy
	Transport *http.Transport
	Alive     atomic.Bool
}

type LoadBalancer struct {
	backends   map[types.ID]*Backend
	backendIDs []types.ID

	current atomic.Uint64
	leader  atomic.Uint64
}

type MetadataStatus struct {
	MetadataInfo struct {
		RaftStatus struct {
			LeaderID string `json:"leader_id"`
			Voters   string `json:"voters"`
		} `json:"raft_status"`
	} `json:"metadata_info"`
}

func main() {
	backendURLs := map[types.ID]string{
		11: "http://127.0.0.1:12277",
		12: "http://127.0.0.1:12278",
		13: "http://127.0.0.1:12279",
	}

	lb := NewLoadBalancer(backendURLs)

	ctx, cancel := signal.NotifyContext(
		context.Background(),
		syscall.SIGINT,
		syscall.SIGTERM,
	)
	defer cancel()

	go lb.startHealthChecks(ctx)
	go lb.startLeaderUpdates(ctx)
	go lb.startIdleCleanup(ctx)

	server := &http.Server{
		Addr:              serverAddr,
		Handler:           corsMiddleware(loggingMiddleware(lb)),
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		log.Printf("Load balancer listening on %s", serverAddr)

		if err := server.ListenAndServe(); err != nil &&
			!errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("Server failed: %v", err)
		}
	}()

	<-ctx.Done()

	log.Println("Shutting down load balancer...")

	shutdownCtx, shutdownCancel := context.WithTimeout(
		context.Background(),
		10*time.Second,
	)
	defer shutdownCancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Printf("Graceful shutdown failed: %v", err)
	}

	lb.CloseIdleConnections()

	log.Println("Load balancer stopped")
}

func NewLoadBalancer(serverURLs map[types.ID]string) *LoadBalancer {
	backends := make(map[types.ID]*Backend, len(serverURLs))

	var initialLeader types.ID

	for id, rawURL := range serverURLs {
		if initialLeader == 0 {
			initialLeader = id
		}

		parsedURL, err := url.Parse(rawURL)
		if err != nil {
			log.Fatalf("Invalid backend URL %s: %v", rawURL, err)
		}

		transport := newTransport()

		backend := &Backend{
			ID:        id,
			URL:       parsedURL,
			Transport: transport,
		}

		backend.Proxy = newReverseProxy(backend)

		backend.Alive.Store(true)

		backends[id] = backend
	}

	lb := &LoadBalancer{
		backends:   backends,
		backendIDs: slices.Collect(maps.Keys(backends)),
	}

	lb.leader.Store(uint64(initialLeader))

	return lb
}

func newTransport() *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,

		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,

		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		MaxConnsPerHost:       50,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
}

func newReverseProxy(backend *Backend) *httputil.ReverseProxy {
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(backend.URL)

			pr.Out.Header.Set(
				"X-Forwarded-Host",
				pr.In.Host,
			)

			pr.Out.Header.Set(
				"X-Forwarded-Proto",
				"http",
			)

			clientIP, _, err := net.SplitHostPort(pr.In.RemoteAddr)
			if err == nil {
				pr.Out.Header.Set(
					"X-Forwarded-For",
					clientIP,
				)
			}
		},

		Transport: backend.Transport,

		FlushInterval: 100 * time.Millisecond,

		ErrorHandler: func(
			w http.ResponseWriter,
			r *http.Request,
			err error,
		) {
			backend.Alive.Store(false)

			log.Printf(
				"Proxy error [%s]: %v",
				backend.URL,
				err,
			)

			http.Error(
				w,
				"Backend unavailable",
				http.StatusBadGateway,
			)
		},
	}

	return proxy
}

func (lb *LoadBalancer) ServeHTTP(
	w http.ResponseWriter,
	r *http.Request,
) {
	if isWriteMethod(r.Method) {
		leaderID := types.ID(lb.leader.Load())

		if backend, ok := lb.backends[leaderID]; ok &&
			backend.Alive.Load() {

			backend.Proxy.ServeHTTP(w, r)
			return
		}
	}

	backend := lb.nextHealthyBackend()

	if backend == nil {
		http.Error(
			w,
			"No healthy backend servers available",
			http.StatusServiceUnavailable,
		)
		return
	}

	backend.Proxy.ServeHTTP(w, r)
}

func (lb *LoadBalancer) nextHealthyBackend() *Backend {
	total := len(lb.backendIDs)

	for i := 0; i < total; i++ {
		idx := lb.current.Add(1)

		backend := lb.backends[
			lb.backendIDs[idx%uint64(total)]
		]

		if backend.Alive.Load() {
			return backend
		}
	}

	return nil
}

func (lb *LoadBalancer) startHealthChecks(ctx context.Context) {
	ticker := time.NewTicker(healthCheckInterval)
	defer ticker.Stop()

	lb.healthCheck()

	for {
		select {
		case <-ctx.Done():
			return

		case <-ticker.C:
			lb.healthCheck()
		}
	}
}

func (lb *LoadBalancer) healthCheck() {
	var wg sync.WaitGroup

	for _, backend := range lb.backends {
		wg.Add(1)

		go func(backend *Backend) {
			defer wg.Done()

			ctx, cancel := context.WithTimeout(
				context.Background(),
				3*time.Second,
			)
			defer cancel()

			req, err := http.NewRequestWithContext(
				ctx,
				http.MethodGet,
				backend.URL.String()+"/health",
				nil,
			)
			if err != nil {
				return
			}

			resp, err := healthClient.Do(req)
			if err != nil {
				backend.Alive.Store(false)

				log.Printf(
					"Health check failed [%s]: %v",
					backend.URL,
					err,
				)

				return
			}

			defer resp.Body.Close()

			alive := resp.StatusCode == http.StatusOK

			old := backend.Alive.Load()

			backend.Alive.Store(alive)

			if old != alive {
				log.Printf(
					"Backend %s changed status: alive=%v",
					backend.URL,
					alive,
				)
			}
		}(backend)
	}

	wg.Wait()
}

func (lb *LoadBalancer) startLeaderUpdates(ctx context.Context) {
	ticker := time.NewTicker(leaderUpdateInterval)
	defer ticker.Stop()

	lb.updateLeaderInfo()

	for {
		select {
		case <-ctx.Done():
			return

		case <-ticker.C:
			lb.updateLeaderInfo()
		}
	}
}

func (lb *LoadBalancer) updateLeaderInfo() {
	var wg sync.WaitGroup

	for _, backend := range lb.backends {
		if !backend.Alive.Load() {
			continue
		}

		wg.Add(1)

		go func(backend *Backend) {
			defer wg.Done()

			ctx, cancel := context.WithTimeout(
				context.Background(),
				3*time.Second,
			)
			defer cancel()

			req, err := http.NewRequestWithContext(
				ctx,
				http.MethodGet,
				backend.URL.String()+"/status",
				nil,
			)
			if err != nil {
				return
			}

			resp, err := healthClient.Do(req)
			if err != nil {
				return
			}

			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				return
			}

			var status MetadataStatus

			if err := json.NewDecoder(resp.Body).
				Decode(&status); err != nil {
				return
			}

			leaderStr := status.
				MetadataInfo.
				RaftStatus.
				LeaderID

			if leaderStr == "" {
				return
			}

			leaderID, err := types.IDFromString(leaderStr)
			if err != nil {
				log.Printf(
					"Invalid leader ID from %s: %v",
					backend.URL,
					err,
				)
				return
			}

			current := types.ID(lb.leader.Load())

			if current != leaderID {
				log.Printf(
					"Leader changed: %s -> %s",
					current,
					leaderID,
				)

				lb.leader.Store(uint64(leaderID))
			}
		}(backend)
	}

	wg.Wait()
}

func (lb *LoadBalancer) startIdleCleanup(ctx context.Context) {
	ticker := time.NewTicker(idleCleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case <-ticker.C:
			lb.CloseIdleConnections()
			log.Println("Closed idle connections")
		}
	}
}

func (lb *LoadBalancer) CloseIdleConnections() {
	for _, backend := range lb.backends {
		backend.Transport.CloseIdleConnections()
	}
}

func isWriteMethod(method string) bool {
	switch method {
	case http.MethodPost,
		http.MethodPut,
		http.MethodPatch,
		http.MethodDelete:
		return true

	default:
		return false
	}
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(
		w http.ResponseWriter,
		r *http.Request,
	) {
		w.Header().Set(
			"Access-Control-Allow-Origin",
			"*",
		)

		w.Header().Set(
			"Access-Control-Allow-Methods",
			"GET, POST, PUT, PATCH, DELETE, OPTIONS",
		)

		w.Header().Set(
			"Access-Control-Allow-Headers",
			"Content-Type, Authorization",
		)

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(
		w http.ResponseWriter,
		r *http.Request,
	) {
		start := time.Now()

		next.ServeHTTP(w, r)

		log.Printf(
			"%s %s %s (%s)",
			r.Method,
			r.URL.Path,
			r.RemoteAddr,
			time.Since(start),
		)
	})
}
