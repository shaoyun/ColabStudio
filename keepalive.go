package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/websocket"
)

const (
	keepAliveModeWSPulse = "ws_pulse"
	keepAlivePulseCode   = "import pathlib, time\npathlib.Path('/tmp/.colabstudio_keepalive').write_text(str(int(time.time())))\n"
)

var (
	errKeepAlivePulseTimeout = errors.New("keepalive pulse reply timeout")

	dialKeepAliveWS = func(ctx context.Context, config *websocket.Config) (*websocket.Conn, error) {
		return config.DialContext(ctx)
	}

	pingFrameCodec = websocket.Codec{
		Marshal: func(v any) ([]byte, byte, error) {
			b, _ := v.([]byte)
			return b, websocket.PingFrame, nil
		},
	}

	rawFrameCodec = websocket.Codec{
		Unmarshal: func(data []byte, _ byte, v any) error {
			if out, ok := v.(*[]byte); ok {
				*out = append((*out)[:0], data...)
			}
			return nil
		},
	}

	// ensureKeepAlive ensures a persistent-compatible keep-alive kernel exists for root.
	// Legacy while-True sleep kernels are gently interrupted and upgraded to ws_pulse.
	ensureKeepAlive = func(p runtimeProxy, root string) error {
		_, err := ensureKeepAliveKernelState(p, root, "", false)
		return err
	}
)

type keepAliveMark struct {
	ID   string `json:"id"`
	Mode string `json:"mode,omitempty"`
}

type keepAliveConfig struct {
	pulseInterval        time.Duration
	replyTimeout         time.Duration
	tunnelInterval       time.Duration
	proxyRefreshInterval time.Duration
	reconnectMin         time.Duration
	reconnectMax         time.Duration
	connectTimeout       time.Duration
	writeTimeout         time.Duration
}

func defaultKeepAliveConfig() keepAliveConfig {
	return keepAliveConfig{
		pulseInterval:        25 * time.Second,
		replyTimeout:         20 * time.Second,
		tunnelInterval:       35 * time.Second,
		proxyRefreshInterval: 8 * time.Minute,
		reconnectMin:         3 * time.Second,
		reconnectMax:         15 * time.Second,
		connectTimeout:       20 * time.Second,
		writeTimeout:         10 * time.Second,
	}
}

type sessionExit struct {
	seq            uint64
	runtime        string
	reconnectNow   bool
	pulseSucceeded bool
	pulseTimeout   bool
	err            error
}

type keepAliveManager struct {
	m       *serviceManager
	cfg     keepAliveConfig
	wakeCh  chan struct{}
	resetCh chan struct{}
	stopCh  chan struct{}
	doneCh  chan struct{}

	stopOnce sync.Once
	wg       sync.WaitGroup
	kernelMu sync.Mutex

	stateMu       sync.Mutex
	ownedRuntime  string
	ownedKernelID string
	acquireWake   func(reason string) func()
}

func newKeepAliveManager(m *serviceManager) *keepAliveManager {
	return newKeepAliveManagerWithConfig(m, defaultKeepAliveConfig())
}

func newKeepAliveManagerWithConfig(m *serviceManager, cfg keepAliveConfig) *keepAliveManager {
	return &keepAliveManager{
		m:       m,
		cfg:     cfg,
		wakeCh:  make(chan struct{}, 1),
		resetCh: make(chan struct{}, 1),
		stopCh:  make(chan struct{}),
		doneCh:  make(chan struct{}),
	}
}

func (k *keepAliveManager) setWakeProvider(fn func(reason string) func()) {
	k.stateMu.Lock()
	k.acquireWake = fn
	k.stateMu.Unlock()
}

func (k *keepAliveManager) getWakeProvider() func(reason string) func() {
	k.stateMu.Lock()
	defer k.stateMu.Unlock()
	return k.acquireWake
}

func (k *keepAliveManager) notify() {
	select {
	case k.wakeCh <- struct{}{}:
	default:
	}
}

func (k *keepAliveManager) forceReconnect() {
	select {
	case k.resetCh <- struct{}{}:
	default:
	}
}

func (k *keepAliveManager) close() {
	k.stopOnce.Do(func() {
		close(k.stopCh)
	})
}

func (k *keepAliveManager) closeAndWait() {
	k.close()
	<-k.doneCh
}

func (k *keepAliveManager) setOwnedKernel(runtime, id string) {
	if runtime == "" {
		return
	}
	k.stateMu.Lock()
	k.ownedRuntime = runtime
	k.ownedKernelID = id
	k.stateMu.Unlock()
}

func (k *keepAliveManager) ownedKernelFor(runtime string) string {
	k.stateMu.Lock()
	defer k.stateMu.Unlock()
	if k.ownedRuntime == runtime {
		return k.ownedKernelID
	}
	return ""
}

func (k *keepAliveManager) clearOwnedKernel(runtime string) {
	k.stateMu.Lock()
	if runtime == "" || k.ownedRuntime == runtime {
		k.ownedRuntime = ""
		k.ownedKernelID = ""
	}
	k.stateMu.Unlock()
}

// syncRoot persists an already-created in-memory keep-alive kernel into RemoteRoot
// once deployment initializes the directory, avoiding creating a duplicate kernel.
func (k *keepAliveManager) syncRoot(p runtimeProxy, runtime, root string) {
	if k == nil || runtime == "" || root == "" {
		return
	}
	k.kernelMu.Lock()
	defer k.kernelMu.Unlock()
	id := k.ownedKernelFor(runtime)
	if !kernelID.MatchString(id) {
		return
	}
	var existing keepAliveMark
	if remoteJSON(p, root, "keepalive.json", &existing) == nil && kernelID.MatchString(existing.ID) {
		return
	}
	b, _ := json.Marshal(keepAliveMark{ID: id, Mode: keepAliveModeWSPulse})
	_ = putRemote(p, root, "keepalive.json", b)
}

func interruptOrRestartKernel(p runtimeProxy, id string) error {
	if err := proxyRequest(httpClient, p, "POST", "api/kernels/"+id+"/interrupt", nil, nil); err == nil {
		return nil
	}
	return proxyRequest(httpClient, p, "POST", "api/kernels/"+id+"/restart", nil, nil)
}

func writeKeepAliveMark(p runtimeProxy, root, id string) error {
	if root == "" {
		return nil
	}
	b, _ := json.Marshal(keepAliveMark{ID: id, Mode: keepAliveModeWSPulse})
	return putRemote(p, root, "keepalive.json", b)
}

func findLiveKernel(kernels []diagnosticKernel, id string) (diagnosticKernel, bool) {
	for _, item := range kernels {
		if item.ID == id && item.State != "dead" {
			return item, true
		}
	}
	return diagnosticKernel{}, false
}

// ensureKeepAliveKernelState finds or creates ColabStudio's dedicated keep-alive kernel.
// It never inspects, interrupts, or deletes kernels not owned by keepalive.json or fallbackID.
func ensureKeepAliveKernelState(p runtimeProxy, root, fallbackID string, recoverBusy bool) (string, error) {
	var kernels []diagnosticKernel
	if e := proxyRequest(httpClient, p, "GET", "api/kernels", nil, &kernels); e != nil {
		return "", e
	}
	if root != "" {
		var mark keepAliveMark
		if remoteJSON(p, root, "keepalive.json", &mark) == nil && kernelID.MatchString(mark.ID) {
			if existing, ok := findLiveKernel(kernels, mark.ID); ok {
				if mark.Mode == keepAliveModeWSPulse {
					if !recoverBusy || existing.State != "busy" {
						return mark.ID, nil
					}
					if interruptOrRestartKernel(p, mark.ID) == nil {
						return mark.ID, nil
					}
				} else {
					if existing.State != "busy" || interruptOrRestartKernel(p, mark.ID) == nil {
						if e := writeKeepAliveMark(p, root, mark.ID); e != nil {
							return "", e
						}
						return mark.ID, nil
					}
				}
				// Only our own marked keep-alive kernel is ever removed when unrecoverable.
				_ = proxyRequest(httpClient, p, "DELETE", "api/kernels/"+mark.ID, nil, nil)
			}
		}
	}
	if kernelID.MatchString(fallbackID) {
		if existing, ok := findLiveKernel(kernels, fallbackID); ok {
			if !recoverBusy || existing.State != "busy" || interruptOrRestartKernel(p, fallbackID) == nil {
				_ = writeKeepAliveMark(p, root, fallbackID)
				return fallbackID, nil
			}
		}
	}
	slow := *httpClient
	slow.Timeout = 3 * time.Minute
	var created diagnosticKernel
	if e := proxyRequest(&slow, p, "POST", "api/kernels", map[string]string{"name": "python3"}, &created); e != nil {
		return "", e
	}
	if !kernelID.MatchString(created.ID) {
		return "", errors.New("保活 kernel 无效")
	}
	if e := writeKeepAliveMark(p, root, created.ID); e != nil {
		return "", e
	}
	return created.ID, nil
}

func (k *keepAliveManager) resolveKeepAliveKernel(p runtimeProxy, runtime, root string, recoverBusy bool) (string, error) {
	k.kernelMu.Lock()
	defer k.kernelMu.Unlock()
	id, err := ensureKeepAliveKernelState(p, root, k.ownedKernelFor(runtime), recoverBusy)
	if err != nil {
		k.clearOwnedKernel(runtime)
		return "", err
	}
	k.setOwnedKernel(runtime, id)
	return id, nil
}

// desiredTarget returns whether keep-alive should run now, whether an auth/identity error
// looks transient (should retry later), and the target runtime name.
func (k *keepAliveManager) desiredTarget() (wantActive bool, transient bool, runtime string) {
	if k.m == nil || k.m.err != nil {
		return false, false, ""
	}
	r := k.m.get()
	if r.Runtime == "" || r.StopRequested {
		return false, false, ""
	}
	if _, err := k.m.identity(r); err != nil {
		if strings.Contains(err.Error(), "绑定账号不同") || strings.Contains(err.Error(), "尚未连接") {
			return false, false, ""
		}
		return false, true, ""
	}
	return true, false, r.Runtime
}

func (k *keepAliveManager) loop() {
	defer close(k.doneCh)

	var (
		activeRuntime string
		sessCancel    context.CancelFunc
		sessSeq       uint64
		retryTimer    *time.Timer
		retryCh       <-chan time.Time
		backoff       time.Duration
		recoverBusy   bool
		releaseWake   func()
	)
	sessExitCh := make(chan sessionExit, 8)

	// App Nap / sleep assertion calls must NEVER occur while any mutex is held,
	// because mygo.Power.KeepAwake and its release func synchronously dispatch to
	// the main UI thread. All acquire/release calls are serialized inside this
	// single lock-free goroutine.
	ensureWakeAcquired := func() {
		if releaseWake != nil {
			return
		}
		if fn := k.getWakeProvider(); fn != nil {
			releaseWake = fn("ColabStudio 云端实例运行保活中")
		}
	}
	ensureWakeReleased := func() {
		if releaseWake != nil {
			releaseWake()
			releaseWake = nil
		}
	}
	stopRetry := func() {
		if retryTimer != nil {
			retryTimer.Stop()
			retryTimer = nil
			retryCh = nil
		}
	}
	scheduleRetry := func(d time.Duration) {
		stopRetry()
		retryTimer = time.NewTimer(d)
		retryCh = retryTimer.C
	}
	cancelSession := func() {
		if sessCancel != nil {
			sessCancel()
			sessCancel = nil
			activeRuntime = ""
			sessSeq++
		}
	}
	startSession := func(runtime string) {
		stopRetry()
		sessSeq++
		seq := sessSeq
		ctx, cancel := context.WithCancel(context.Background())
		sessCancel = cancel
		activeRuntime = runtime
		rec := recoverBusy
		k.wg.Add(1)
		go func() {
			defer k.wg.Done()
			reconnectNow, pulsed, pulseTimeout, err := k.runSession(ctx, runtime, rec)
			sessExitCh <- sessionExit{
				seq:            seq,
				runtime:        runtime,
				reconnectNow:   reconnectNow,
				pulseSucceeded: pulsed,
				pulseTimeout:   pulseTimeout,
				err:            err,
			}
		}()
	}

	reconcile := func() {
		wantActive, transient, target := k.desiredTarget()
		if !wantActive {
			cancelSession()
			backoff = 0
			recoverBusy = false
			k.clearOwnedKernel("")
			ensureWakeReleased()
			if transient {
				scheduleRetry(nextKeepAliveBackoff(&backoff, k.cfg))
			} else {
				stopRetry()
			}
			return
		}
		ensureWakeAcquired()
		if activeRuntime != "" && activeRuntime != target {
			cancelSession()
			backoff = 0
			recoverBusy = false
			k.clearOwnedKernel("")
		}
		if sessCancel == nil && retryCh == nil {
			startSession(target)
		}
	}

	reconcile()

	for {
		select {
		case <-k.stopCh:
			cancelSession()
			stopRetry()
			allDone := make(chan struct{})
			go func() {
				k.wg.Wait()
				close(allDone)
			}()
		drain:
			for {
				select {
				case <-sessExitCh:
				case <-allDone:
					break drain
				}
			}
			ensureWakeReleased()
			return

		case <-k.wakeCh:
			reconcile()

		case <-k.resetCh:
			cancelSession()
			stopRetry()
			backoff = 0
			recoverBusy = false
			reconcile()

		case <-retryCh:
			retryTimer = nil
			retryCh = nil
			reconcile()

		case res := <-sessExitCh:
			if res.seq != sessSeq {
				continue
			}
			sessCancel = nil
			activeRuntime = ""
			if res.pulseTimeout {
				recoverBusy = true
			} else if res.pulseSucceeded {
				recoverBusy = false
			}
			wantActive, transient, target := k.desiredTarget()
			if !wantActive {
				backoff = 0
				recoverBusy = false
				k.clearOwnedKernel("")
				ensureWakeReleased()
				if transient {
					scheduleRetry(nextKeepAliveBackoff(&backoff, k.cfg))
				}
				continue
			}
			ensureWakeAcquired()
			if res.reconnectNow {
				backoff = 0
				recoverBusy = false
				startSession(target)
				continue
			}
			if res.pulseSucceeded {
				backoff = 0
			}
			scheduleRetry(nextKeepAliveBackoff(&backoff, k.cfg))
		}
	}
}

func nextKeepAliveBackoff(cur *time.Duration, cfg keepAliveConfig) time.Duration {
	if *cur <= 0 {
		*cur = cfg.reconnectMin
	} else {
		*cur *= 2
		if *cur > cfg.reconnectMax {
			*cur = cfg.reconnectMax
		}
	}
	return *cur
}

func (k *keepAliveManager) runSession(ctx context.Context, runtime string, recoverBusy bool) (reconnectNow bool, pulseSucceeded bool, pulseTimeout bool, err error) {
	if ctx.Err() != nil {
		return false, false, false, ctx.Err()
	}

	r := k.m.get()
	_ = k.m.keepAlive(r)

	p, err := k.m.studio.proxy(runtime)
	if err != nil {
		return false, false, false, err
	}
	if ctx.Err() != nil {
		return false, false, false, ctx.Err()
	}

	r = k.m.get()
	if r.Runtime != runtime || r.StopRequested {
		return false, false, false, nil
	}
	kid, err := k.resolveKeepAliveKernel(p, runtime, r.RemoteRoot, recoverBusy)
	if err != nil {
		return false, false, false, err
	}
	if ctx.Err() != nil {
		return false, false, false, ctx.Err()
	}

	session := random()
	wsConfig, err := diagnosticSocketConfig(p, kid, session)
	if err != nil {
		return false, false, false, err
	}
	dialCtx, dialCancel := context.WithTimeout(ctx, k.cfg.connectTimeout)
	ws, err := dialKeepAliveWS(dialCtx, wsConfig)
	dialCancel()
	if err != nil {
		return false, false, false, err
	}
	ws.MaxPayloadBytes = 1 << 20

	var closeOnce sync.Once
	closeWS := func() {
		closeOnce.Do(func() {
			_ = ws.SetDeadline(time.Now())
			_ = ws.Close()
		})
	}
	defer closeWS()

	ackCh := make(chan string, 64)
	readErrCh := make(chan error, 1)
	readIdleTimeout := k.cfg.pulseInterval*3 + k.cfg.replyTimeout
	if readIdleTimeout < 2*time.Second {
		readIdleTimeout = 2 * time.Second
	}

	go func() {
		var raw []byte
		for {
			_ = ws.SetReadDeadline(time.Now().Add(readIdleTimeout))
			if recvErr := rawFrameCodec.Receive(ws, &raw); recvErr != nil {
				readErrCh <- recvErr
				return
			}
			var msg struct {
				Parent struct {
					ID string `json:"msg_id"`
				} `json:"parent_header"`
				Header struct {
					Type string `json:"msg_type"`
				} `json:"header"`
				Content struct {
					State string `json:"execution_state"`
				} `json:"content"`
			}
			if json.Unmarshal(raw, &msg) == nil && msg.Parent.ID != "" {
				if msg.Header.Type == "execute_reply" || (msg.Header.Type == "status" && msg.Content.State == "idle") {
					select {
					case ackCh <- msg.Parent.ID:
					default:
					}
				}
			}
		}
	}()

	sendPulse := func() error {
		for {
			select {
			case <-ackCh:
			default:
				goto drained
			}
		}
	drained:
		_ = ws.SetWriteDeadline(time.Now().Add(k.cfg.writeTimeout))
		if e := pingFrameCodec.Send(ws, []byte("keepalive")); e != nil {
			return e
		}
		msgID := random()
		payload := map[string]any{
			"header": map[string]any{
				"msg_id":   msgID,
				"username": "colabstudio",
				"session":  session,
				"date":     time.Now().UTC().Format(time.RFC3339Nano),
				"msg_type": "execute_request",
				"version":  "5.3",
			},
			"parent_header": map[string]any{},
			"metadata":      map[string]any{},
			"channel":       "shell",
			"content": map[string]any{
				"code":             keepAlivePulseCode,
				"silent":           false,
				"store_history":    false,
				"user_expressions": map[string]any{},
				"allow_stdin":      false,
				"stop_on_error":    false,
			},
			"buffers": []any{},
		}
		_ = ws.SetWriteDeadline(time.Now().Add(k.cfg.writeTimeout))
		if e := websocket.JSON.Send(ws, payload); e != nil {
			return e
		}
		wait := time.NewTimer(k.cfg.replyTimeout)
		defer wait.Stop()
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case rErr := <-readErrCh:
				if rErr == nil {
					rErr = errors.New("keepalive ws closed")
				}
				return rErr
			case ackID := <-ackCh:
				if ackID == msgID {
					return nil
				}
			case <-wait.C:
				return errKeepAlivePulseTimeout
			}
		}
	}

	if e := sendPulse(); e != nil {
		return false, pulseSucceeded, errors.Is(e, errKeepAlivePulseTimeout), e
	}
	pulseSucceeded = true

	pulseTicker := time.NewTicker(k.cfg.pulseInterval)
	defer pulseTicker.Stop()
	tunnelTicker := time.NewTicker(k.cfg.tunnelInterval)
	defer tunnelTicker.Stop()
	refreshTicker := time.NewTicker(k.cfg.proxyRefreshInterval)
	defer refreshTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return false, pulseSucceeded, false, ctx.Err()
		case rErr := <-readErrCh:
			if rErr == nil {
				rErr = errors.New("keepalive ws closed")
			}
			return false, pulseSucceeded, false, rErr
		case <-pulseTicker.C:
			if e := sendPulse(); e != nil {
				return false, pulseSucceeded, errors.Is(e, errKeepAlivePulseTimeout), e
			}
		case <-tunnelTicker.C:
			cur := k.m.get()
			if cur.Runtime != runtime || cur.StopRequested {
				return false, pulseSucceeded, false, nil
			}
			_ = k.m.keepAlive(cur)
		case <-refreshTicker.C:
			latest, pErr := k.m.studio.proxy(runtime)
			if pErr == nil && (latest.URL != p.URL || latest.Token != p.Token) {
				return true, pulseSucceeded, false, nil
			}
		}
	}
}
