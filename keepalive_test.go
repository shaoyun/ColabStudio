package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/websocket"
)

type pipeListener struct {
	ch     chan net.Conn
	closed chan struct{}
	once   sync.Once
}

func newPipeListener() *pipeListener {
	return &pipeListener{
		ch:     make(chan net.Conn),
		closed: make(chan struct{}),
	}
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.closed:
		return nil, errors.New("pipe listener closed")
	}
}

func (l *pipeListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *pipeListener) Addr() net.Addr { return pipeAddr{} }

type pipeAddr struct{}

func (pipeAddr) Network() string { return "pipe" }
func (pipeAddr) String() string  { return "pipe" }

// TestKeepAliveLeavesRunningKernel verifies both modern ws_pulse reuse and
// seamless upgrade of legacy while-True busy keep-alive kernels without ever
// touching user kernels.
func TestKeepAliveLeavesRunningKernel(t *testing.T) {
	t.Run("reuses_ws_pulse_kernel_and_ignores_user_kernel", func(t *testing.T) {
		old := httpClient
		t.Cleanup(func() { httpClient = old })
		httpClient = &http.Client{Transport: serviceRoundTrip(func(r *http.Request) (*http.Response, error) {
			switch {
			case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "api/kernels"):
				return wakeResponse(`[{"id":"user-kernel","execution_state":"idle"},{"id":"mine-kernel","execution_state":"idle"}]`), nil
			case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "keepalive.json"):
				return wakeResponse(`{"format":"text","content":"{\"id\":\"mine-kernel\",\"mode\":\"ws_pulse\"}"}`), nil
			}
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
			return nil, nil
		})}
		if e := ensureKeepAlive(runtimeProxy{URL: "https://x.r.colab.dev", Token: "t"}, "content/root"); e != nil {
			t.Fatal(e)
		}
	})

	t.Run("upgrades_legacy_busy_kernel_via_interrupt_without_touching_user_kernel", func(t *testing.T) {
		old := httpClient
		t.Cleanup(func() { httpClient = old })
		interrupted := false
		var written keepAliveMark
		httpClient = &http.Client{Transport: serviceRoundTrip(func(r *http.Request) (*http.Response, error) {
			switch {
			case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "api/kernels"):
				return wakeResponse(`[{"id":"user-kernel","execution_state":"busy"},{"id":"mine-kernel","execution_state":"busy"}]`), nil
			case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "keepalive.json"):
				return wakeResponse(`{"format":"text","content":"{\"id\":\"mine-kernel\"}"}`), nil
			case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "api/kernels/mine-kernel/interrupt"):
				interrupted = true
				return &http.Response{StatusCode: 204, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}, nil
			case r.Method == "PUT" && strings.HasSuffix(r.URL.Path, "keepalive.json"):
				var putReq struct {
					Content string `json:"content"`
					Format  string `json:"format"`
				}
				raw, _ := io.ReadAll(r.Body)
				_ = json.Unmarshal(raw, &putReq)
				decoded, _ := base64.StdEncoding.DecodeString(putReq.Content)
				_ = json.Unmarshal(decoded, &written)
				return wakeResponse(`{}`), nil
			}
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
			return nil, nil
		})}
		if e := ensureKeepAlive(runtimeProxy{URL: "https://x.r.colab.dev", Token: "t"}, "content/root"); e != nil {
			t.Fatal(e)
		}
		if !interrupted {
			t.Fatal("expected legacy busy keep-alive kernel to be interrupted")
		}
		if written.ID != "mine-kernel" || written.Mode != keepAliveModeWSPulse {
			t.Fatalf("unexpected written keepalive mark: %+v", written)
		}
	})
}

func TestTunnelKeepAliveSendsColabCliHeader(t *testing.T) {
	var gotAuth, gotTunnel, gotAgent, gotAccept, gotPath, gotQuery string
	m := wakeFixture(t, func(r *http.Request) (*http.Response, error) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotAuth = r.Header.Get("Authorization")
		gotTunnel = r.Header.Get("X-Colab-Tunnel")
		gotAgent = r.Header.Get("X-Colab-Client-Agent")
		gotAccept = r.Header.Get("Accept")
		return wakeResponse(`{}`), nil
	})
	if e := m.keepAlive(ServiceRecord{Runtime: "compat/b3duZWQ"}); e != nil {
		t.Fatalf("keepAlive failed: %v", e)
	}
	if !strings.HasSuffix(gotPath, "/owned/keep-alive/") || gotQuery != "authuser=0" {
		t.Fatalf("unexpected keep-alive target: path=%q query=%q", gotPath, gotQuery)
	}
	if gotAuth != "Bearer test-access" || gotTunnel != "Google" || gotAgent != "colab-cli" || gotAccept != "application/json" {
		t.Fatalf("unexpected headers: auth=%q tunnel=%q agent=%q accept=%q", gotAuth, gotTunnel, gotAgent, gotAccept)
	}
}

func TestTickDoesNotShortCircuitInspectOrStopOnKeepAliveError(t *testing.T) {
	binding := accountBinding("test@example.com")
	envelope := func(v any) *http.Response {
		b, _ := json.Marshal(v)
		wrap, _ := json.Marshal(map[string]string{"format": "text", "content": string(b)})
		return wakeResponse(string(wrap))
	}

	t.Run("transient_keepalive_error_keeps_online_state", func(t *testing.T) {
		m := wakeFixture(t, func(req *http.Request) (*http.Response, error) {
			switch {
			case strings.HasSuffix(req.URL.Path, "assignments"):
				return wakeResponse(`{"assignments":[{"endpoint":"owned","runtimeProxyInfo":{"url":"https://owned.r.colab.dev","token":"proxy-secret"}}]}`), nil
			case strings.HasSuffix(req.URL.Path, "/keep-alive/"):
				resp := wakeResponse(`forbidden`)
				resp.StatusCode = 403
				return resp, nil
			case strings.HasSuffix(req.URL.Path, "state.json"):
				return envelope(remoteState{
					Owner:     "owned",
					Binding:   binding,
					Version:   toyboxVersion,
					Stage:     "online",
					Health:    true,
					URL:       "https://demo.trycloudflare.com",
					Heartbeat: float64(time.Now().Unix()),
				}), nil
			case strings.HasSuffix(req.URL.Path, "api/kernels"):
				return wakeResponse(`[{"id":"ka-1","execution_state":"idle"}]`), nil
			case strings.HasSuffix(req.URL.Path, "keepalive.json"):
				return wakeResponse(`{"format":"text","content":"{\"id\":\"ka-1\",\"mode\":\"ws_pulse\"}"}`), nil
			default:
				t.Fatalf("unexpected request: %s %s", req.Method, req.URL.Path)
				return nil, nil
			}
		})
		m.record = ServiceRecord{
			ID:         "owned",
			Account:    "test@example.com",
			Binding:    binding,
			Runtime:    "compat/b3duZWQ",
			RemoteRoot: "content/.colabstudio/owned",
			State:      "online",
		}

		now := time.Now()
		m.tick(now, now.Add(-30*time.Second))
		got := m.get()
		if got.State != "online" {
			t.Fatalf("expected state to stay online despite keep-alive HTTP error, got %q (%q)", got.State, got.Message)
		}
	})

	t.Run("keepalive_error_does_not_block_finishStop", func(t *testing.T) {
		drainSent := false
		m := wakeFixture(t, func(req *http.Request) (*http.Response, error) {
			switch {
			case strings.HasSuffix(req.URL.Path, "assignments"):
				return wakeResponse(`{"assignments":[{"endpoint":"owned","runtimeProxyInfo":{"url":"https://owned.r.colab.dev","token":"proxy-secret"}}]}`), nil
			case strings.HasSuffix(req.URL.Path, "/keep-alive/"):
				resp := wakeResponse(`error`)
				resp.StatusCode = 500
				return resp, nil
			case strings.HasSuffix(req.URL.Path, "state.json"):
				return envelope(remoteState{
					Owner:     "owned",
					Binding:   binding,
					Version:   toyboxVersion,
					Stage:     "online",
					Busy:      true,
					Health:    true,
					Heartbeat: float64(time.Now().Unix()),
				}), nil
			case req.Method == "PUT" && strings.HasSuffix(req.URL.Path, "command.json"):
				drainSent = true
				return wakeResponse(`{}`), nil
			default:
				t.Fatalf("unexpected request: %s %s", req.Method, req.URL.Path)
				return nil, nil
			}
		})
		m.record = ServiceRecord{
			ID:            "owned",
			Account:       "test@example.com",
			Binding:       binding,
			Runtime:       "compat/b3duZWQ",
			RemoteRoot:    "content/.colabstudio/owned",
			State:         "online",
			StopRequested: true,
		}

		now := time.Now()
		m.tick(now, now.Add(-30*time.Second))
		got := m.get()
		if !drainSent || got.State != "draining" {
			t.Fatalf("expected finishStop to issue drain despite keep-alive error: drain=%v state=%q", drainSent, got.State)
		}
	})
}

func TestPersistentKeepAliveWebSocketLifecycle(t *testing.T) {
	var (
		mu             sync.Mutex
		proxyToken     = "token-v1"
		tokensSeen     []string
		connOpened     int32
		activeConns    int32
		pulseCount     int32
		activeWSConns  = map[*websocket.Conn]struct{}{}
		wakeAcquireCnt int32
		wakeReleaseCnt int32
		pulsesThreeCh  = make(chan struct{}, 1)
		secondTokenCh  = make(chan struct{}, 1)
		dropFirstOnce  sync.Once
		reconnectCh    = make(chan struct{}, 1)
	)

	ln := newPipeListener()
	t.Cleanup(func() { _ = ln.Close() })

	wsHandler := websocket.Server{
		Handshake: func(_ *websocket.Config, req *http.Request) error {
			mu.Lock()
			expectedToken := proxyToken
			mu.Unlock()
			gotHeaderToken := req.Header.Get("X-Colab-Runtime-Proxy-Token")
			gotAgent := req.Header.Get("X-Colab-Client-Agent")
			gotQueryToken := req.URL.Query().Get("colab-runtime-proxy-token")
			if gotAgent != "vscode" || gotHeaderToken == "" || gotQueryToken != gotHeaderToken {
				return errors.New("missing proxy handshake headers/query")
			}
			mu.Lock()
			tokensSeen = append(tokensSeen, gotHeaderToken)
			if gotHeaderToken == "token-v2" {
				select {
				case secondTokenCh <- struct{}{}:
				default:
				}
			}
			_ = expectedToken
			mu.Unlock()
			return nil
		},
		Handler: func(ws *websocket.Conn) {
			mu.Lock()
			activeWSConns[ws] = struct{}{}
			mu.Unlock()
			defer func() {
				mu.Lock()
				delete(activeWSConns, ws)
				mu.Unlock()
				_ = ws.Close()
				atomic.AddInt32(&activeConns, -1)
			}()

			atomic.AddInt32(&activeConns, 1)
			connNum := atomic.AddInt32(&connOpened, 1)
			if connNum == 2 {
				select {
				case reconnectCh <- struct{}{}:
				default:
				}
			}

			for {
				var req struct {
					Header struct {
						ID   string `json:"msg_id"`
						Type string `json:"msg_type"`
					} `json:"header"`
					Content struct {
						Code         string `json:"code"`
						Silent       bool   `json:"silent"`
						StoreHistory bool   `json:"store_history"`
					} `json:"content"`
				}
				if err := websocket.JSON.Receive(ws, &req); err != nil {
					return
				}
				if req.Header.Type != "execute_request" || req.Content.Silent || req.Content.StoreHistory {
					t.Errorf("unexpected pulse frame: %+v", req)
					return
				}
				n := atomic.AddInt32(&pulseCount, 1)
				if n >= 3 {
					select {
					case pulsesThreeCh <- struct{}{}:
					default:
					}
				}
				_ = websocket.JSON.Send(ws, map[string]any{
					"parent_header": map[string]any{"msg_id": req.Header.ID},
					"header":        map[string]any{"msg_type": "execute_reply"},
					"content":       map[string]any{"status": "ok"},
				})
				_ = websocket.JSON.Send(ws, map[string]any{
					"parent_header": map[string]any{"msg_id": req.Header.ID},
					"header":        map[string]any{"msg_type": "status"},
					"content":       map[string]any{"execution_state": "idle"},
				})
			}
		},
	}

	srv := &http.Server{Handler: wsHandler}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		mu.Lock()
		for c := range activeWSConns {
			_ = c.Close()
		}
		mu.Unlock()
		_ = srv.Close()
	})

	oldDial := dialKeepAliveWS
	t.Cleanup(func() { dialKeepAliveWS = oldDial })
	dialKeepAliveWS = func(ctx context.Context, config *websocket.Config) (*websocket.Conn, error) {
		cConn, sConn := net.Pipe()
		select {
		case ln.ch <- sConn:
		case <-ln.closed:
			_ = cConn.Close()
			_ = sConn.Close()
			return nil, errors.New("server closed")
		case <-ctx.Done():
			_ = cConn.Close()
			_ = sConn.Close()
			return nil, ctx.Err()
		}
		return websocket.NewClient(config, cConn)
	}

	m := wakeFixture(t, func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "assignments"):
			mu.Lock()
			tok := proxyToken
			mu.Unlock()
			return wakeResponse(`{"assignments":[{"endpoint":"owned","runtimeProxyInfo":{"url":"https://owned.r.colab.dev","token":"` + tok + `"}}]}`), nil
		case strings.HasSuffix(r.URL.Path, "/keep-alive/"):
			return wakeResponse(`{}`), nil
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "api/kernels"):
			return wakeResponse(`[{"id":"ka-kernel","execution_state":"idle"}]`), nil
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "api/kernels"):
			return wakeResponse(`{"id":"ka-kernel","execution_state":"idle"}`), nil
		default:
			t.Fatalf("unexpected HTTP request: %s %s", r.Method, r.URL.Path)
			return nil, nil
		}
	})

	cfg := keepAliveConfig{
		pulseInterval:        20 * time.Millisecond,
		replyTimeout:         200 * time.Millisecond,
		tunnelInterval:       25 * time.Millisecond,
		proxyRefreshInterval: 45 * time.Millisecond,
		reconnectMin:         10 * time.Millisecond,
		reconnectMax:         30 * time.Millisecond,
		connectTimeout:       time.Second,
		writeTimeout:         time.Second,
	}
	ka := newKeepAliveManagerWithConfig(m, cfg)
	ka.setWakeProvider(func(string) func() {
		atomic.AddInt32(&wakeAcquireCnt, 1)
		return func() {
			atomic.AddInt32(&wakeReleaseCnt, 1)
		}
	})
	m.ka = ka
	m.record = ServiceRecord{
		ID:      "owned",
		Account: "test@example.com",
		Binding: accountBinding("test@example.com"),
		Runtime: "compat/b3duZWQ",
		State:   "allocated",
	}

	go ka.loop()
	t.Cleanup(func() { ka.closeAndWait() })
	ka.notify()

	// 1. Wait until >= 3 pulses complete while maintaining the SAME single persistent connection.
	select {
	case <-pulsesThreeCh:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for 3 keep-alive pulses (pulses=%d)", atomic.LoadInt32(&pulseCount))
	}
	if opened := atomic.LoadInt32(&connOpened); opened != 1 {
		t.Fatalf("expected 1 persistent connection across 3 pulses, got %d", opened)
	}
	if got := atomic.LoadInt32(&wakeAcquireCnt); got != 1 {
		t.Fatalf("expected 1 wake assertion acquisition, got %d", got)
	}

	// 2. Force-close current server connection -> verify automatic reconnect.
	dropFirstOnce.Do(func() {
		mu.Lock()
		for c := range activeWSConns {
			_ = c.Close()
		}
		mu.Unlock()
	})
	select {
	case <-reconnectCh:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for automatic reconnect after server drop (opened=%d)", atomic.LoadInt32(&connOpened))
	}

	// 3. Rotate proxy token -> verify refresh cycle reconnects immediately with token-v2.
	mu.Lock()
	proxyToken = "token-v2"
	mu.Unlock()
	select {
	case <-secondTokenCh:
	case <-time.After(3 * time.Second):
		mu.Lock()
		seen := append([]string(nil), tokensSeen...)
		mu.Unlock()
		t.Fatalf("timed out waiting for reconnect with rotated proxy token (tokensSeen=%v)", seen)
	}

	// 4. Request stop -> verify persistent WS closes and wake assertion is released.
	r := m.get()
	r.StopRequested = true
	if e := m.put(r); e != nil {
		t.Fatal(e)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&activeConns) == 0 && atomic.LoadInt32(&wakeReleaseCnt) >= atomic.LoadInt32(&wakeAcquireCnt) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if ac := atomic.LoadInt32(&activeConns); ac != 0 {
		t.Fatalf("expected 0 active WebSocket connections after stop, got %d", ac)
	}
	acq, rel := atomic.LoadInt32(&wakeAcquireCnt), atomic.LoadInt32(&wakeReleaseCnt)
	if acq == 0 || rel != acq {
		t.Fatalf("mismatched wake assertions: acquired=%d released=%d", acq, rel)
	}
}
