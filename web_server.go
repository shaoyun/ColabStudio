package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	sessionCookieName      = "colabstudio_session"
	sessionCookieTTL       = 24 * time.Hour
	defaultListenAddr      = "127.0.0.1:8787"
	authFailureLimit       = 12
	authFailureWindow      = time.Minute
	maxAuthBucketEntries   = 1024
	defaultWaitTimeoutSec  = 600
	maxWaitTimeoutSec      = 1800
	maxRESTBodyBytes       = 64 * 1024
)

type authKind int

const (
	authNone authKind = iota
	authHeader
	authCookie
)

type authFailureBucket struct {
	count   int
	resetAt time.Time
}

type authFailureLimiter struct {
	mu      sync.Mutex
	limit   int
	window  time.Duration
	buckets map[string]authFailureBucket
}

func newAuthFailureLimiter(limit int, window time.Duration) *authFailureLimiter {
	return &authFailureLimiter{
		limit:   limit,
		window:  window,
		buckets: make(map[string]authFailureBucket),
	}
}

func (l *authFailureLimiter) isBlocked(ip string, now time.Time) bool {
	if ip == "" {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[ip]
	if !ok {
		return false
	}
	if !now.Before(b.resetAt) {
		delete(l.buckets, ip)
		return false
	}
	return b.count >= l.limit
}

func (l *authFailureLimiter) recordFailure(ip string, now time.Time) {
	if ip == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.buckets) >= maxAuthBucketEntries {
		for k, v := range l.buckets {
			if !now.Before(v.resetAt) {
				delete(l.buckets, k)
			}
		}
	}
	b := l.buckets[ip]
	if !now.Before(b.resetAt) {
		b = authFailureBucket{count: 0, resetAt: now.Add(l.window)}
	}
	b.count++
	l.buckets[ip] = b
}

type webServer struct {
	studio       *Studio
	tokenHash    [32]byte
	sessionKey   [32]byte
	limiter      *authFailureLimiter
	pollInterval time.Duration
	version      string
	mux          *http.ServeMux
}

func newWebServer(studio *Studio, token string) (*webServer, error) {
	if studio == nil {
		return nil, errors.New("studio 实例不能为空")
	}
	if err := validateAPIToken(token); err != nil {
		return nil, err
	}
	tokenHash := sha256.Sum256([]byte(token))
	mac := hmac.New(sha256.New, tokenHash[:])
	mac.Write([]byte("colabstudio-web-session-v1"))
	var sessionKey [32]byte
	copy(sessionKey[:], mac.Sum(nil))

	srv := &webServer{
		studio:       studio,
		tokenHash:    tokenHash,
		sessionKey:   sessionKey,
		limiter:      newAuthFailureLimiter(authFailureLimit, authFailureWindow),
		pollInterval: time.Second,
		version:      toyboxVersion,
	}
	srv.mux = srv.buildMux()
	return srv, nil
}

func (srv *webServer) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			h.Set("Cache-Control", "no-store")
		}
		srv.mux.ServeHTTP(w, r)
	})
}

func (srv *webServer) buildMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", srv.handleIndex)
	mux.HandleFunc("GET /api/v1/health", srv.handleHealth)
	mux.HandleFunc("GET /api/auth/session", srv.handleAuthSession)
	mux.HandleFunc("POST /api/auth/login", srv.handleAuthLogin)
	mux.Handle("POST /api/auth/logout", srv.requireAuth(http.HandlerFunc(srv.handleAuthLogout)))

	mux.Handle("POST /api/rpc/{method}", srv.requireAuth(http.HandlerFunc(srv.handleRPC)))
	mux.Handle("GET /api/v1/cu", srv.requireAuth(http.HandlerFunc(srv.handleCU)))
	mux.Handle("GET /api/v1/usage", srv.requireAuth(http.HandlerFunc(srv.handleCU)))
	mux.Handle("GET /api/v1/status", srv.requireAuth(http.HandlerFunc(srv.handleStatus)))
	mux.Handle("POST /api/v1/toybox/start", srv.requireAuth(http.HandlerFunc(srv.handleToyboxStart)))
	mux.Handle("POST /api/v1/toybox/stop", srv.requireAuth(http.HandlerFunc(srv.handleToyboxStop)))
	mux.Handle("POST /api/v1/toybox/wake", srv.requireAuth(http.HandlerFunc(srv.handleToyboxWake)))
	mux.Handle("POST /api/v1/toybox/retry", srv.requireAuth(http.HandlerFunc(srv.handleToyboxRetry)))
	mux.Handle("GET /api/v1/toybox/progress", srv.requireAuth(http.HandlerFunc(srv.handleToyboxProgress)))
	mux.Handle("GET /api/v1/toybox/logs", srv.requireAuth(http.HandlerFunc(srv.handleToyboxLogs)))
	mux.Handle("GET /api/v1/toybox/credentials", srv.requireAuth(http.HandlerFunc(srv.handleToyboxCredentials)))
	mux.Handle("GET /api/v1/runtimes", srv.requireAuth(http.HandlerFunc(srv.handleRuntimes)))
	mux.Handle("POST /api/v1/runtimes/release", srv.requireAuth(http.HandlerFunc(srv.handleRuntimesRelease)))
	return mux
}

func envBool(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func shouldRunWebServer(args []string) bool {
	if envBool("COLABSTUDIO_SERVE") {
		return true
	}
	for _, a := range args {
		if a == "--" {
			break
		}
		switch strings.ToLower(strings.TrimSpace(a)) {
		case "-serve", "--serve", "-serve=true", "--serve=true", "-serve=1", "--serve=1":
			return true
		}
	}
	return false
}

func configureServerStore(headlessVault bool) {
	var backend keyStore
	if headlessVault {
		backend = newFileKeyStore(dataDir())
	} else {
		backend = defaultKeyStore()
	}
	store.mu.Lock()
	store.path = vaultPath()
	store.backend = backend
	store.loaded = false
	store.loadErr = nil
	store.key = nil
	store.values = nil
	store.mu.Unlock()
}

func validateAPIToken(token string) error {
	trimmed := strings.TrimSpace(token)
	if len(trimmed) < 12 {
		return errors.New("API Token 长度至少需要 12 个字符")
	}
	if strings.ContainsAny(trimmed, " \t\r\n") {
		return errors.New("API Token 不能包含空白字符")
	}
	return nil
}

func generateAPIToken() (string, error) {
	var buf [24]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return "cs_" + hex.EncodeToString(buf[:]), nil
}

func writeFile0600Atomic(target string, data []byte) error {
	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, target); err != nil {
		return err
	}
	cleanup = false
	return nil
}

func resolveServerToken(explicitToken, dir string, rotate bool) (token, source string, generated bool, err error) {
	trimmedExplicit := strings.TrimSpace(explicitToken)
	if !rotate && trimmedExplicit != "" {
		if err := validateAPIToken(trimmedExplicit); err != nil {
			return "", "", false, err
		}
		return trimmedExplicit, "环境变量/命令行参数", false, nil
	}
	tokenFile := filepath.Join(dir, "api.token")
	if !rotate {
		raw, rerr := os.ReadFile(tokenFile)
		if rerr == nil {
			existing := strings.TrimSpace(string(raw))
			if err := validateAPIToken(existing); err != nil {
				return "", "", false, fmt.Errorf("已存在的 Token 文件无效 (%s): %w", tokenFile, err)
			}
			return existing, tokenFile, false, nil
		}
		if !os.IsNotExist(rerr) {
			return "", "", false, rerr
		}
	}
	fresh, err := generateAPIToken()
	if err != nil {
		return "", "", false, err
	}
	if err := writeFile0600Atomic(tokenFile, []byte(fresh+"\n")); err != nil {
		return "", "", false, err
	}
	return fresh, tokenFile, true, nil
}

func hasExplicitServeFlag(args []string) bool {
	for _, a := range args {
		if a == "--" {
			break
		}
		switch strings.ToLower(strings.TrimSpace(a)) {
		case "-serve", "--serve", "-serve=true", "--serve=true", "-serve=1", "--serve=1":
			return true
		}
	}
	return false
}

func runWebServerCLI(args []string) int {
	fs := flag.NewFlagSet("colabstudio-server", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	serveFlag := fs.Bool("serve", false, "以 Web 服务模式启动")
	listenAddr := fs.String("listen", envOr("COLABSTUDIO_LISTEN", defaultListenAddr), "HTTP 监听地址")
	dataDirFlag := fs.String("data-dir", "", "数据与凭据目录")
	headlessVault := fs.Bool("headless-vault", envBool("COLABSTUDIO_HEADLESS_VAULT"), "使用文件密钥库替代系统钥匙串")
	tokenFlag := fs.String("token", os.Getenv("COLABSTUDIO_API_TOKEN"), "指定访问控制接口的 API Token")
	printToken := fs.Bool("print-token", false, "打印当前 API Token")
	rotateToken := fs.Bool("rotate-token", false, "轮换生成新的 API Token")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if strings.TrimSpace(*dataDirFlag) != "" {
		if err := setDataDir(*dataDirFlag); err != nil {
			fmt.Fprintf(os.Stderr, "无效的数据目录: %v\n", err)
			return 2
		}
	}

	setWebServerMode(true)
	configureServerStore(*headlessVault)

	dir := dataDir()
	token, source, generated, err := resolveServerToken(*tokenFlag, dir, *rotateToken)
	if err != nil {
		fmt.Fprintf(os.Stderr, "初始化 API Token 失败: %v\n", err)
		return 1
	}

	if *rotateToken || *printToken {
		fmt.Println(token)
		if !*serveFlag && !hasExplicitServeFlag(args) {
			return 0
		}
	}

	srv, err := newWebServer(activeStudio, token)
	if err != nil {
		fmt.Fprintf(os.Stderr, "初始化 Web 服务失败: %v\n", err)
		return 1
	}

	// Ensure the background Toybox service manager / scheduler resumes on boot.
	_ = activeStudio.ServiceStatus()

	httpSrv := &http.Server{
		Addr:              strings.TrimSpace(*listenAddr),
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      120 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	if generated {
		log.Printf("ColabStudio Web 已生成新 API Token 并保存至 %s（执行 -print-token 查看）", source)
	} else {
		log.Printf("ColabStudio Web 已加载 API Token（来源：%s）", source)
	}
	log.Printf("ColabStudio Web 监听地址: http://%s（数据目录: %s）", httpSrv.Addr, dir)

	errCh := make(chan error, 1)
	go func() {
		if serr := httpSrv.ListenAndServe(); serr != nil && !errors.Is(serr, http.ErrServerClosed) {
			errCh <- serr
		}
		close(errCh)
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	select {
	case err := <-errCh:
		if err != nil {
			fmt.Fprintf(os.Stderr, "Web 服务异常退出: %v\n", err)
			return 1
		}
		return 0
	case sig := <-sigCh:
		log.Printf("收到信号 %v，正在优雅关闭 Web 服务...", sig)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(ctx)
		return 0
	}
}

func (srv *webServer) matchesToken(candidate string) bool {
	trimmed := strings.TrimSpace(candidate)
	if trimmed == "" {
		return false
	}
	sum := sha256.Sum256([]byte(trimmed))
	return subtle.ConstantTimeCompare(sum[:], srv.tokenHash[:]) == 1
}

func (srv *webServer) signSessionCookie(now time.Time) (string, error) {
	var nonce [12]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	issued := now.Unix()
	exp := now.Add(sessionCookieTTL).Unix()
	payload := fmt.Sprintf("%d.%d.%s", issued, exp, hex.EncodeToString(nonce[:]))
	mac := hmac.New(sha256.New, srv.sessionKey[:])
	mac.Write([]byte(payload))
	return payload + "." + hex.EncodeToString(mac.Sum(nil)), nil
}

func (srv *webServer) verifySessionCookie(raw string, now time.Time) bool {
	parts := strings.Split(strings.TrimSpace(raw), ".")
	if len(parts) != 4 {
		return false
	}
	issued, err1 := strconv.ParseInt(parts[0], 10, 64)
	exp, err2 := strconv.ParseInt(parts[1], 10, 64)
	if err1 != nil || err2 != nil || len(parts[2]) != 24 {
		return false
	}
	sig, err := hex.DecodeString(parts[3])
	if err != nil || len(sig) != sha256.Size {
		return false
	}
	payload := parts[0] + "." + parts[1] + "." + parts[2]
	mac := hmac.New(sha256.New, srv.sessionKey[:])
	mac.Write([]byte(payload))
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return false
	}
	nowUnix := now.Unix()
	if nowUnix > exp || issued > nowUnix+60 || exp-issued > int64(sessionCookieTTL.Seconds())+60 {
		return false
	}
	return true
}

func clientIP(r *http.Request) string {
	raw := strings.TrimSpace(r.RemoteAddr)
	if host, _, err := net.SplitHostPort(raw); err == nil && host != "" {
		return host
	}
	return raw
}

func hasQueryTokenParam(r *http.Request) bool {
	q := r.URL.Query()
	for _, key := range []string{"token", "api_token", "access_token"} {
		if _, ok := q[key]; ok {
			return true
		}
	}
	return false
}

func extractHeaderToken(r *http.Request) (token string, present bool, validFormat bool) {
	authHdr := strings.TrimSpace(r.Header.Get("Authorization"))
	apiHdr := strings.TrimSpace(r.Header.Get("X-API-Token"))
	if authHdr == "" && apiHdr == "" {
		return "", false, true
	}
	var bearerTok string
	if authHdr != "" {
		if len(authHdr) <= 7 || !strings.EqualFold(authHdr[:7], "Bearer ") {
			return "", true, false
		}
		bearerTok = strings.TrimSpace(authHdr[7:])
		if bearerTok == "" {
			return "", true, false
		}
	}
	if apiHdr != "" && bearerTok != "" && apiHdr != bearerTok {
		return "", true, false
	}
	if apiHdr != "" {
		return apiHdr, true, true
	}
	return bearerTok, true, true
}

func isMutatingMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

func checkCookieCSRF(r *http.Request) bool {
	if r.Header.Get("X-Requested-With") != "ColabStudio" {
		return false
	}
	if origin := strings.TrimSpace(r.Header.Get("Origin")); origin != "" {
		if strings.EqualFold(origin, "null") {
			return false
		}
		u, err := url.Parse(origin)
		if err != nil || u.Host == "" || !strings.EqualFold(u.Host, r.Host) {
			return false
		}
	} else if ref := strings.TrimSpace(r.Header.Get("Referer")); ref != "" {
		u, err := url.Parse(ref)
		if err != nil || u.Host == "" || !strings.EqualFold(u.Host, r.Host) {
			return false
		}
	}
	return true
}

func isHTTPSRequest(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https")
}

func (srv *webServer) authenticate(r *http.Request, countMissing bool) (authKind, int, string) {
	now := time.Now()
	ip := clientIP(r)
	if srv.limiter.isBlocked(ip, now) {
		return authNone, http.StatusTooManyRequests, "认证失败次数过多，请稍后再试"
	}
	if hasQueryTokenParam(r) {
		srv.limiter.recordFailure(ip, now)
		return authNone, http.StatusUnauthorized, "禁止在 URL 查询参数中传递 Token，请使用 Authorization: Bearer 或 X-API-Token 请求头"
	}
	if hdrTok, present, validFmt := extractHeaderToken(r); present {
		if !validFmt || !srv.matchesToken(hdrTok) {
			srv.limiter.recordFailure(ip, now)
			return authNone, http.StatusUnauthorized, "无效的 API Token"
		}
		return authHeader, 0, ""
	}
	if cookie, err := r.Cookie(sessionCookieName); err == nil {
		if !srv.verifySessionCookie(cookie.Value, now) {
			srv.limiter.recordFailure(ip, now)
			return authNone, http.StatusUnauthorized, "网页会话已过期或无效，请重新输入 Token 登录"
		}
		if isMutatingMethod(r.Method) && !checkCookieCSRF(r) {
			return authNone, http.StatusForbidden, "CSRF 校验失败：缺少安全请求头或请求来源不匹配"
		}
		return authCookie, 0, ""
	}
	if countMissing {
		srv.limiter.recordFailure(ip, now)
	}
	return authNone, http.StatusUnauthorized, "未提供认证凭据，请先输入 Token 或携带 Bearer Token"
}

func (srv *webServer) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, status, msg := srv.authenticate(r, true)
		if status != 0 {
			if status == http.StatusTooManyRequests {
				w.Header().Set("Retry-After", "60")
			} else if status == http.StatusUnauthorized {
				w.Header().Set("WWW-Authenticate", `Bearer realm="ColabStudio"`)
			}
			writeJSONError(w, status, msg)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{
		"ok":    false,
		"error": msg,
	})
}

func decodeOptionalJSONBody(w http.ResponseWriter, r *http.Request, maxBytes int64, dst any, allowEmpty bool) error {
	if r.Body == nil {
		if allowEmpty {
			return nil
		}
		return errors.New("请求体不能为空")
	}
	defer r.Body.Close()
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return errors.New("请求体过大")
		}
		return errors.New("读取请求体失败")
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		if allowEmpty {
			return nil
		}
		return errors.New("请求体不能为空")
	}
	return unmarshalRPCArg(trimmed, dst)
}

func composeWebPage() ([]byte, error) {
	htmlBytes, err := assets.ReadFile("web/index.html")
	if err != nil {
		return nil, err
	}
	cssBytes, err := assets.ReadFile("web/style.css")
	if err != nil {
		return nil, err
	}
	bridgeBytes, err := assets.ReadFile("web/web-bridge.js")
	if err != nil {
		return nil, err
	}
	appBytes, err := assets.ReadFile("web/app.js")
	if err != nil {
		return nil, err
	}
	telemetryBytes, err := assets.ReadFile("web/telemetry.js")
	if err != nil {
		return nil, err
	}
	iconBytes, err := assets.ReadFile("web/icon.png")
	if err != nil {
		return nil, err
	}

	pageHTML := strings.Replace(string(htmlBytes), "img-src data:", "img-src data:; connect-src 'self'", 1)
	combinedJS := string(bridgeBytes) + "\n;\n" + string(appBytes) + "\n;\n" + string(telemetryBytes)
	iconDataURI := "data:image/png;base64," + base64.StdEncoding.EncodeToString(iconBytes)

	page := strings.NewReplacer(
		"/*__APP_CSS__*/", string(cssBytes),
		"/*__APP_JS__*/", combinedJS,
		"__APP_ICON__", iconDataURI,
	).Replace(pageHTML)
	return []byte(page), nil
}

func (srv *webServer) handleIndex(w http.ResponseWriter, r *http.Request) {
	page, err := composeWebPage()
	if err != nil {
		http.Error(w, "加载网页资源失败", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(page)
}

func (srv *webServer) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"status":  "ok",
		"mode":    "web",
		"version": srv.version,
	})
}

func (srv *webServer) handleAuthSession(w http.ResponseWriter, r *http.Request) {
	kind, status, msg := srv.authenticate(r, false)
	if status == http.StatusTooManyRequests {
		w.Header().Set("Retry-After", "60")
		writeJSONError(w, status, msg)
		return
	}
	if status != 0 || kind == authNone {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":            true,
			"authenticated": false,
		})
		return
	}
	method := "cookie"
	if kind == authHeader {
		method = "header"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":            true,
		"authenticated": true,
		"method":        method,
	})
}

func (srv *webServer) handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	ip := clientIP(r)
	if srv.limiter.isBlocked(ip, now) {
		w.Header().Set("Retry-After", "60")
		writeJSONError(w, http.StatusTooManyRequests, "认证失败次数过多，请稍后再试")
		return
	}
	if hasQueryTokenParam(r) {
		srv.limiter.recordFailure(ip, now)
		writeJSONError(w, http.StatusUnauthorized, "禁止在 URL 查询参数中传递 Token")
		return
	}
	var req struct {
		Token string `json:"token"`
	}
	if err := decodeOptionalJSONBody(w, r, maxRESTBodyBytes, &req, false); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !srv.matchesToken(req.Token) {
		srv.limiter.recordFailure(ip, now)
		writeJSONError(w, http.StatusUnauthorized, "API Token 无效")
		return
	}
	val, err := srv.signSessionCookie(now)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "签发会话凭据失败")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    val,
		Path:     "/",
		MaxAge:   int(sessionCookieTTL.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   isHTTPSRequest(r),
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":            true,
		"authenticated": true,
	})
}

func (srv *webServer) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   isHTTPSRequest(r),
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":            true,
		"authenticated": false,
	})
}

func (srv *webServer) handleCU(w http.ResponseWriter, _ *http.Request) {
	usage, err := srv.studio.Usage()
	if err != nil {
		writeJSONError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	var remainingHours *float64
	if usage.Balance != nil && usage.Rate != nil && *usage.Rate > 0 {
		v := *usage.Balance / *usage.Rate
		remainingHours = &v
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":                      true,
		"currentBalance":          usage.Balance,
		"consumptionRateHourly":   usage.Rate,
		"assignmentsCount":        usage.Assignments,
		"estimatedRemainingHours": remainingHours,
	})
}

func (srv *webServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	auth := srv.studio.Status()
	svc := srv.studio.ServiceStatus()
	drv := srv.studio.DriveStatus()
	tunnel, _ := srv.studio.TunnelConfigStatus()
	resp := map[string]any{
		"ok":      true,
		"mode":    "web",
		"auth":    auth,
		"service": svc,
		"drive":   drv,
		"tunnel":  tunnel,
	}
	full := strings.TrimSpace(r.URL.Query().Get("full"))
	if full == "1" || strings.EqualFold(full, "true") {
		if usage, err := srv.studio.Usage(); err == nil {
			resp["usage"] = usage
		}
		if rts, err := srv.studio.Runtimes(); err == nil {
			resp["runtimes"] = rts
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func isToyboxBootingState(state string) bool {
	switch state {
	case "starting", "allocating", "allocated", "installing":
		return true
	default:
		return false
	}
}

func isToyboxStoppingState(state string) bool {
	switch state {
	case "draining", "backing_up", "backup_ready", "releasing":
		return true
	default:
		return false
	}
}

func clampWaitTimeout(seconds int) time.Duration {
	if seconds <= 0 {
		seconds = defaultWaitTimeoutSec
	}
	if seconds > maxWaitTimeoutSec {
		seconds = maxWaitTimeoutSec
	}
	return time.Duration(seconds) * time.Second
}

func (srv *webServer) waitForToyboxState(ctx context.Context, timeout time.Duration, check func(ServiceStatus) (bool, error)) (ServiceStatus, error) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	interval := srv.pollInterval
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		st := srv.studio.ServiceStatus()
		done, err := check(st)
		if done || err != nil {
			return st, err
		}
		select {
		case <-ctx.Done():
			return st, ctx.Err()
		case <-deadline.C:
			return st, errors.New("等待 Toybox 状态变更超时")
		case <-ticker.C:
		}
	}
}

func (srv *webServer) handleToyboxStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Accelerator    string `json:"accelerator"`
		Wait           bool   `json:"wait"`
		TimeoutSeconds int    `json:"timeoutSeconds"`
	}
	if err := decodeOptionalJSONBody(w, r, maxRESTBodyBytes, &req, true); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	st := srv.studio.ServiceStatus()
	if acc := strings.TrimSpace(req.Accelerator); acc != "" && !strings.EqualFold(acc, st.Accelerator) {
		if err := srv.studio.SaveMachineType(acc, true); err != nil {
			writeJSONError(w, http.StatusConflict, err.Error())
			return
		}
		st = srv.studio.ServiceStatus()
	}
	if st.State == "online" && !st.StopRequested && st.URL != "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":             true,
			"alreadyRunning": true,
			"started":        false,
			"service":        st,
		})
		return
	}

	started := false
	if !(isToyboxBootingState(st.State) && !st.StopRequested) {
		if err := srv.studio.StartToybox(true); err != nil {
			writeJSONError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		started = true
	}

	if !req.Wait {
		writeJSON(w, http.StatusAccepted, map[string]any{
			"ok":             true,
			"alreadyRunning": false,
			"started":        started,
			"inProgress":     true,
			"service":        srv.studio.ServiceStatus(),
		})
		return
	}

	startedAt := time.Now()
	sawActive := isToyboxBootingState(st.State) || st.Operating
	finalSt, err := srv.waitForToyboxState(r.Context(), clampWaitTimeout(req.TimeoutSeconds), func(cur ServiceStatus) (bool, error) {
		if cur.State == "online" && !cur.StopRequested && cur.URL != "" {
			return true, nil
		}
		if isToyboxBootingState(cur.State) || cur.Operating {
			sawActive = true
			return false, nil
		}
		if cur.State == "lost" || cur.State == "disconnected" {
			return true, errors.New(firstNonEmpty(cur.StartupError, cur.Message, "Toybox 启动失败"))
		}
		if !cur.Operating && cur.State == "stopped" && (sawActive || time.Since(startedAt) > 3*time.Second) {
			return true, errors.New(firstNonEmpty(cur.StartupError, cur.Message, "Toybox 启动失败"))
		}
		return false, nil
	})
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"ok":      false,
			"error":   err.Error(),
			"service": finalSt,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":             true,
		"alreadyRunning": false,
		"started":        started,
		"service":        finalSt,
	})
}

func (srv *webServer) handleToyboxStop(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Force          bool `json:"force"`
		Wait           bool `json:"wait"`
		TimeoutSeconds int  `json:"timeoutSeconds"`
	}
	if err := decodeOptionalJSONBody(w, r, maxRESTBodyBytes, &req, true); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	st := srv.studio.ServiceStatus()
	if (st.State == "stopped" || st.State == "") && st.Runtime == "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":             true,
			"alreadyStopped": true,
			"stopped":        false,
			"service":        st,
		})
		return
	}
	if req.Force {
		if err := srv.studio.forceReleaseToybox(); err != nil {
			writeJSONError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":      true,
			"forced":  true,
			"stopped": true,
			"service": srv.studio.ServiceStatus(),
		})
		return
	}

	if !st.StopRequested && !isToyboxStoppingState(st.State) {
		if err := srv.studio.StopToybox(true); err != nil {
			writeJSONError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
	}
	st = srv.studio.ServiceStatus()
	if (st.State == "stopped" || st.State == "") && st.Runtime == "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":      true,
			"forced":  false,
			"stopped": true,
			"service": st,
		})
		return
	}
	if !req.Wait {
		writeJSON(w, http.StatusAccepted, map[string]any{
			"ok":       true,
			"forced":   false,
			"stopping": true,
			"service":  st,
		})
		return
	}
	finalSt, err := srv.waitForToyboxState(r.Context(), clampWaitTimeout(req.TimeoutSeconds), func(cur ServiceStatus) (bool, error) {
		if (cur.State == "stopped" || cur.State == "") && cur.Runtime == "" {
			return true, nil
		}
		return false, nil
	})
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"ok":      false,
			"error":   err.Error(),
			"service": finalSt,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"forced":  false,
		"stopped": true,
		"service": finalSt,
	})
}

func (srv *webServer) handleToyboxWake(w http.ResponseWriter, _ *http.Request) {
	if err := srv.studio.WakeToybox(true); err != nil {
		writeJSONError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"ok":      true,
		"service": srv.studio.ServiceStatus(),
	})
}

func (srv *webServer) handleToyboxRetry(w http.ResponseWriter, _ *http.Request) {
	if err := srv.studio.RetryToybox(true); err != nil {
		writeJSONError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"ok":      true,
		"service": srv.studio.ServiceStatus(),
	})
}

func (srv *webServer) handleToyboxProgress(w http.ResponseWriter, _ *http.Request) {
	p, err := srv.studio.DeploymentProgress()
	if err != nil {
		writeJSONError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"progress": p,
	})
}

func (srv *webServer) handleToyboxLogs(w http.ResponseWriter, r *http.Request) {
	kind := strings.TrimSpace(r.URL.Query().Get("kind"))
	if kind == "" {
		kind = "deployment"
	}
	logs, err := srv.studio.ToyboxLogs(kind)
	if err != nil {
		writeJSONError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":   true,
		"kind": kind,
		"logs": logs,
	})
}

func (srv *webServer) handleToyboxCredentials(w http.ResponseWriter, _ *http.Request) {
	access, err := srv.studio.ToyboxAccessStatus()
	if err != nil {
		writeJSONError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	password, _ := srv.studio.ToyboxPassword()
	apiToken, _ := srv.studio.ToyboxAPIToken()
	apiBaseURL, _ := srv.studio.toyboxAPIBaseURL()
	webURL, _ := srv.studio.toyboxVerifiedURL()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"url":        webURL,
		"password":   password,
		"apiBaseURL": apiBaseURL,
		"apiToken":   apiToken,
		"access":     access,
	})
}

func (srv *webServer) handleRuntimes(w http.ResponseWriter, _ *http.Request) {
	rts, err := srv.studio.Runtimes()
	if err != nil {
		writeJSONError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":            true,
		"runtimes":      rts,
		"toyboxRuntime": srv.studio.ServiceStatus().Runtime,
	})
}

func (srv *webServer) handleRuntimesRelease(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name  string `json:"name"`
		All   bool   `json:"all"`
		Force bool   `json:"force"`
	}
	if err := decodeOptionalJSONBody(w, r, maxRESTBodyBytes, &req, false); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" && !req.All {
		writeJSONError(w, http.StatusBadRequest, "请指定要释放的实例 name，或传入 all:true")
		return
	}
	if req.Name != "" && req.All {
		writeJSONError(w, http.StatusBadRequest, "name 与 all 不能同时指定")
		return
	}

	managed := srv.studio.ServiceStatus().Runtime
	if req.Name != "" {
		if managed != "" && req.Name == managed {
			if !req.Force {
				writeJSONError(w, http.StatusConflict, "当前实例为 Toybox 托管运行中实例；请改用 POST /api/v1/toybox/stop 安全停机，或显式传 force:true 强制释放")
				return
			}
			if err := srv.studio.forceReleaseToybox(); err != nil {
				writeJSONError(w, http.StatusUnprocessableEntity, err.Error())
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"ok":       true,
				"forced":   true,
				"released": []string{req.Name},
			})
			return
		}
		if err := srv.studio.Release(req.Name, true); err != nil {
			writeJSONError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":       true,
			"released": []string{req.Name},
		})
		return
	}

	rts, err := srv.studio.Runtimes()
	if err != nil {
		writeJSONError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	released := make([]string, 0, len(rts))
	skipped := make([]map[string]string, 0)
	failed := make([]map[string]string, 0)
	for _, rt := range rts {
		if managed != "" && rt.Name == managed {
			if !req.Force {
				skipped = append(skipped, map[string]string{
					"name":   rt.Name,
					"reason": "Toybox 托管实例受保护（未传 force:true，已跳过）",
				})
				continue
			}
			if err := srv.studio.forceReleaseToybox(); err != nil {
				failed = append(failed, map[string]string{
					"name":  rt.Name,
					"error": err.Error(),
				})
			} else {
				released = append(released, rt.Name)
			}
			continue
		}
		if err := srv.studio.Release(rt.Name, true); err != nil {
			failed = append(failed, map[string]string{
				"name":  rt.Name,
				"error": err.Error(),
			})
		} else {
			released = append(released, rt.Name)
		}
	}
	status := http.StatusOK
	if len(failed) > 0 {
		status = http.StatusUnprocessableEntity
	}
	writeJSON(w, status, map[string]any{
		"ok":       len(failed) == 0,
		"released": released,
		"skipped":  skipped,
		"failed":   failed,
	})
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
