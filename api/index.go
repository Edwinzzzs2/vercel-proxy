package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	githubRepoURL    = "https://github.com/tbxark/vercel-proxy"
	identityEncoding = "identity"
	proxyAuthHeader  = "X-Proxy-Token"
	defaultLogLimit  = 200
	defaultLogTZ     = "Asia/Shanghai"

	corsAllowOrigin  = "*"
	corsAllowMethods = "POST, GET, OPTIONS, PUT, DELETE"
	corsAllowHeaders = "Content-Type, Content-Length, Accept-Encoding, X-CSRF-Token, Authorization, X-PROXY-HOST, X-PROXY-SCHEME, X-Proxy-Token"
)

var (
	proxyURLPattern = regexp.MustCompile(`^/*(https?:)/*`)
	defaultProxy    = mustNewProxy(ApplyEnvConfig(Config{}))
)

// Config controls proxy behavior without reading environment variables.
type Config struct {
	// AuthToken requires callers to provide the same value in X-Proxy-Token.
	// 为空时采用安全默认值：除 AuthWhitelist 外的目标全部拒绝访问。
	AuthToken string `json:"authToken,omitempty"`

	// AuthWhitelist 允许匹配的代理目标跳过鉴权。
	// 规则与 DomainWhitelist 一致，支持子域名、通配符、排除项和端口。
	AuthWhitelist []string `json:"authWhitelist,omitempty"`

	// Socks5Proxy routes all outbound upstream requests through a SOCKS5 proxy.
	// It accepts either "host:port" or a "socks5://host:port" / "socks5h://host:port" URL.
	Socks5Proxy string `json:"socks5Proxy,omitempty"`

	// DomainWhitelist limits target hosts. Empty means all domains are allowed.
	// Entries match the exact domain and its subdomains. Wildcard characters * and ? are supported.
	// Prefix an entry with - to exclude it. Suffix :port limits the entry to that port; :0 allows any port.
	DomainWhitelist []string `json:"domainWhitelist,omitempty"`

	// DisableCompression asks upstream servers for an uncompressed response.
	DisableCompression bool `json:"disableCompression,omitempty"`

	// DisableGlobalCORS disables proxy-managed CORS headers for all responses.
	DisableGlobalCORS bool `json:"disableGlobalCors,omitempty"`

	// LogPassword enables the /logs page. Empty keeps the page disabled.
	LogPassword string `json:"logPassword,omitempty"`

	// LogLimit controls how many recent proxy requests are kept in memory.
	LogLimit int `json:"logLimit,omitempty"`

	// LogTimezone controls how timestamps are rendered on the /logs page.
	LogTimezone string `json:"logTimezone,omitempty"`
}

// Proxy is a configurable reverse proxy handler.
type Proxy struct {
	client             *http.Client
	authToken          string
	authWhitelist      []domainRule
	domainWhitelist    []domainRule
	disableCompression bool
	globalCORS         bool
	logPassword        string
	logLocation        *time.Location
	logTimezone        string
	requestLogs        *requestLogStore
}

// NewProxy creates a reusable proxy handler with explicit configuration.
func NewProxy(config Config) (*Proxy, error) {
	client, err := newHTTPClient(config.Socks5Proxy)
	if err != nil {
		return nil, err
	}
	logLimit := config.LogLimit
	if logLimit <= 0 {
		logLimit = defaultLogLimit
	}
	logLocation, logTimezone := loadLogLocation(config.LogTimezone)
	proxy := &Proxy{
		client:             client,
		authToken:          config.AuthToken,
		authWhitelist:      normalizeDomainWhitelist(config.AuthWhitelist),
		domainWhitelist:    normalizeDomainWhitelist(config.DomainWhitelist),
		disableCompression: config.DisableCompression,
		globalCORS:         !config.DisableGlobalCORS,
		logPassword:        config.LogPassword,
		logLocation:        logLocation,
		logTimezone:        logTimezone,
		requestLogs:        newRequestLogStore(logLimit),
	}
	proxy.client.CheckRedirect = proxy.checkRedirect

	return proxy, nil
}

func mustNewProxy(config Config) *Proxy {
	proxy, err := NewProxy(config)
	if err != nil {
		panic(err)
	}
	return proxy
}

// ApplyEnvConfig 将 Docker 或 Vercel 的运行时环境变量覆盖到配置中。
func ApplyEnvConfig(config Config) Config {
	if authToken, ok := os.LookupEnv("PROXY_AUTH_TOKEN"); ok {
		config.AuthToken = strings.TrimSpace(authToken)
	}
	if whitelist, ok := os.LookupEnv("PROXY_AUTH_WHITELIST"); ok {
		config.AuthWhitelist = splitCommaSeparated(whitelist)
	}
	if whitelist, ok := os.LookupEnv("PROXY_DOMAIN_WHITELIST"); ok {
		config.DomainWhitelist = splitCommaSeparated(whitelist)
	}
	if password, ok := os.LookupEnv("PROXY_LOG_PASSWORD"); ok {
		config.LogPassword = strings.TrimSpace(password)
	}
	if limit, ok := os.LookupEnv("PROXY_LOG_LIMIT"); ok {
		if parsed, err := strconv.Atoi(strings.TrimSpace(limit)); err == nil {
			config.LogLimit = parsed
		}
	}
	if timezone, ok := os.LookupEnv("PROXY_LOG_TIMEZONE"); ok {
		config.LogTimezone = strings.TrimSpace(timezone)
	}
	return config
}

func loadLogLocation(name string) (*time.Location, string) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = defaultLogTZ
	}
	location, err := time.LoadLocation(name)
	if err != nil {
		log.Printf("Invalid PROXY_LOG_TIMEZONE %q, fallback to UTC: %v", name, err)
		return time.UTC, "UTC"
	}
	return location, name
}

func splitCommaSeparated(value string) []string {
	parts := strings.Split(value, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if entry := strings.TrimSpace(part); entry != "" {
			result = append(result, entry)
		}
	}
	return result
}

func internalServerError(w http.ResponseWriter, err error) {
	if err != nil {
		log.Printf("Internal server error: %v", err.Error())
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func Handler(w http.ResponseWriter, r *http.Request) {
	defaultProxy.ServeHTTP(w, r)
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	startedAt := time.Now()
	defer func() {
		if err := recover(); err != nil {
			log.Printf("WithHandler panic: %v", err)
			http.Error(w, fmt.Sprintf("internal server error: %v", err), http.StatusInternalServerError)
		}
	}()

	if p.globalCORS {
		setCORSHeaders(w)

		// Handle the OPTIONS preflight request
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}
	}

	// Redirect to the GitHub repository
	if r.URL.Path == "/" {
		http.Redirect(w, r, githubRepoURL, http.StatusMovedPermanently)
		return
	}
	if r.URL.Path == "/favicon.ico" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.URL.Path == "/logs" {
		p.serveLogsPage(w, r)
		return
	}
	// Get the URL to proxy
	rawURL := proxyURL(r)
	targetURL, err := parseTargetURL(rawURL)
	if err != nil {
		http.Error(w, "invalid url: "+rawURL, http.StatusBadRequest)
		p.recordRequestLog(r, nil, http.StatusBadRequest, "invalid_url", err.Error(), false, false, time.Since(startedAt))
		return
	}
	if err := p.checkDomain(targetURL); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		p.recordRequestLog(r, targetURL, http.StatusForbidden, "domain_blocked", err.Error(), false, false, time.Since(startedAt))
		return
	}
	authorized := p.isAuthorized(r.Header.Get(proxyAuthHeader))
	authWhitelisted := p.isAuthWhitelisted(targetURL)
	if !authorized && !authWhitelisted {
		writeUnauthorized(w)
		p.recordRequestLog(r, targetURL, http.StatusUnauthorized, "unauthorized", "missing or invalid proxy token", authorized, authWhitelisted, time.Since(startedAt))
		return
	}

	// Create a new request
	// 记录本次请求是否已鉴权，避免公开目标通过重定向访问受保护目标。
	ctx := context.WithValue(r.Context(), authorizationContextKey{}, authorized)
	req, err := http.NewRequestWithContext(ctx, r.Method, targetURL.String(), r.Body)
	if err != nil {
		internalServerError(w, err)
		return
	}
	copyHeaders(r.Header, req.Header)
	// The proxy credential is only for this service and must never reach upstream.
	req.Header.Del(proxyAuthHeader)
	if p.disableCompression {
		disableUpstreamCompression(req.Header)
	}

	// Send the request to the real server
	resp, err := p.client.Do(req)
	if err != nil {
		var domainErr *domainNotAllowedError
		if errors.As(err, &domainErr) {
			http.Error(w, domainErr.Error(), http.StatusForbidden)
			p.recordRequestLog(r, targetURL, http.StatusForbidden, "redirect_domain_blocked", domainErr.Error(), authorized, authWhitelisted, time.Since(startedAt))
			return
		}
		var authErr *authenticationRequiredError
		if errors.As(err, &authErr) {
			writeUnauthorized(w)
			p.recordRequestLog(r, targetURL, http.StatusUnauthorized, "redirect_unauthorized", authErr.Error(), authorized, authWhitelisted, time.Since(startedAt))
			return
		}
		internalServerError(w, err)
		p.recordRequestLog(r, targetURL, http.StatusInternalServerError, "upstream_error", err.Error(), authorized, authWhitelisted, time.Since(startedAt))
		return
	}
	defer closeResponseBody(resp)

	if err := proxyRaw(w, resp, r, p.globalCORS); err != nil {
		log.Printf("Proxy response error: %v", err)
		p.recordRequestLog(r, targetURL, resp.StatusCode, "copy_error", err.Error(), authorized, authWhitelisted, time.Since(startedAt))
		return
	}
	p.recordRequestLog(r, targetURL, resp.StatusCode, "proxied", resp.Status, authorized, authWhitelisted, time.Since(startedAt))
}

func writeUnauthorized(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"error":"unauthorized","details":"missing or invalid X-Proxy-Token"}`))
}

func (p *Proxy) serveLogsPage(w http.ResponseWriter, r *http.Request) {
	if p.logPassword == "" {
		http.NotFound(w, r)
		return
	}
	if !isValidBasicAuthPassword(r, p.logPassword) {
		w.Header().Set("WWW-Authenticate", `Basic realm="Proxy Logs"`)
		http.Error(w, "proxy logs require password", http.StatusUnauthorized)
		return
	}

	entries := p.requestLogs.snapshot()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = fmt.Fprint(w, renderLogsPage(entries, p.logLocation, p.logTimezone))
}

func isValidBasicAuthPassword(r *http.Request, expectedPassword string) bool {
	_, password, ok := r.BasicAuth()
	if !ok {
		return false
	}
	expectedHash := sha256.Sum256([]byte(expectedPassword))
	actualHash := sha256.Sum256([]byte(password))
	return subtle.ConstantTimeCompare(expectedHash[:], actualHash[:]) == 1
}

func (p *Proxy) recordRequestLog(r *http.Request, targetURL *url.URL, status int, result, detail string, authorized, authWhitelisted bool, duration time.Duration) {
	if p.requestLogs == nil {
		return
	}
	p.requestLogs.add(proxyRequestLog{
		Time:            time.Now(),
		Method:          r.Method,
		ClientIP:        clientIP(r),
		Target:          sanitizeTargetURLForLog(targetURL),
		TargetHost:      targetHostForLog(targetURL),
		Status:          status,
		Result:          result,
		Detail:          detail,
		Authorized:      authorized,
		AuthWhitelisted: authWhitelisted,
		Duration:        duration,
	})
}

func clientIP(r *http.Request) string {
	if forwardedFor := r.Header.Get("X-Forwarded-For"); forwardedFor != "" {
		return strings.TrimSpace(strings.Split(forwardedFor, ",")[0])
	}
	if realIP := r.Header.Get("X-Real-IP"); realIP != "" {
		return strings.TrimSpace(realIP)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}

func targetHostForLog(targetURL *url.URL) string {
	if targetURL == nil {
		return ""
	}
	return targetURL.Host
}

func sanitizeTargetURLForLog(targetURL *url.URL) string {
	if targetURL == nil {
		return ""
	}
	safeURL := *targetURL
	query := safeURL.Query()
	for key := range query {
		if isSensitiveQueryKey(key) {
			query.Set(key, "[REDACTED]")
		}
	}
	safeURL.RawQuery = query.Encode()
	return safeURL.String()
}

func isSensitiveQueryKey(key string) bool {
	key = strings.ToLower(key)
	for _, pattern := range []string{"token", "authorization", "password", "secret", "sig", "jwt", "key"} {
		if strings.Contains(key, pattern) {
			return true
		}
	}
	return false
}

type proxyRequestLog struct {
	Time            time.Time
	Method          string
	ClientIP        string
	Target          string
	TargetHost      string
	Status          int
	Result          string
	Detail          string
	Authorized      bool
	AuthWhitelisted bool
	Duration        time.Duration
}

type requestLogStore struct {
	mu      sync.Mutex
	limit   int
	entries []proxyRequestLog
}

func newRequestLogStore(limit int) *requestLogStore {
	if limit <= 0 {
		limit = defaultLogLimit
	}
	return &requestLogStore{limit: limit}
}

func (s *requestLogStore) add(entry proxyRequestLog) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.entries = append(s.entries, entry)
	if overflow := len(s.entries) - s.limit; overflow > 0 {
		s.entries = append([]proxyRequestLog(nil), s.entries[overflow:]...)
	}
}

func (s *requestLogStore) snapshot() []proxyRequestLog {
	s.mu.Lock()
	defer s.mu.Unlock()

	// 页面默认展示最新记录在最上方，排查刚发生的请求更快。
	result := make([]proxyRequestLog, 0, len(s.entries))
	for i := len(s.entries) - 1; i >= 0; i-- {
		result = append(result, s.entries[i])
	}
	return result
}

func renderLogsPage(entries []proxyRequestLog, location *time.Location, timezone string) string {
	if location == nil {
		location = time.UTC
		timezone = "UTC"
	}
	var b strings.Builder
	b.WriteString(`<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Proxy Logs</title><style>`)
	b.WriteString(`body{margin:0;font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;background:#f6f7f9;color:#1f2937}main{padding:24px}.header{display:flex;align-items:center;justify-content:space-between;gap:16px;margin:0 0 8px}h1{margin:0;font-size:24px}.refresh{border:1px solid #d1d5db;border-radius:6px;background:#fff;color:#1f2937;padding:8px 12px;cursor:pointer}.summary{margin:0 0 20px;color:#6b7280}.table-wrap{overflow:auto;background:#fff;border:1px solid #e5e7eb;border-radius:8px}table{border-collapse:collapse;width:100%;min-width:1080px}th,td{padding:10px 12px;border-bottom:1px solid #edf0f3;text-align:left;font-size:13px;vertical-align:top}th{background:#f9fafb;color:#4b5563;font-weight:600;position:sticky;top:0}.url{max-width:420px;word-break:break-all}.ok{color:#047857}.warn{color:#b45309}.bad{color:#b91c1c}.empty{padding:40px;text-align:center;color:#6b7280}.pill{display:inline-block;border-radius:999px;padding:2px 8px;background:#eef2ff;color:#3730a3;font-size:12px}</style></head><body><main>`)
	b.WriteString(`<div class="header"><h1>Proxy Logs</h1><button class="refresh" onclick="location.reload()">Refresh</button></div>`)
	b.WriteString(`<p class="summary">Showing latest `)
	b.WriteString(strconv.Itoa(len(entries)))
	b.WriteString(` requests. Timezone: `)
	b.WriteString(html.EscapeString(timezone))
	b.WriteString(`.</p>`)
	if len(entries) == 0 {
		b.WriteString(`<div class="table-wrap"><div class="empty">No proxy requests recorded yet.</div></div>`)
		b.WriteString(`</main></body></html>`)
		return b.String()
	}

	b.WriteString(`<div class="table-wrap"><table><thead><tr><th>Time</th><th>Status</th><th>Result</th><th>Method</th><th>Host</th><th>Target</th><th>Auth</th><th>IP</th><th>Duration</th><th>Detail</th></tr></thead><tbody>`)
	for _, entry := range entries {
		statusClass := "ok"
		if entry.Status >= 500 {
			statusClass = "bad"
		} else if entry.Status >= 400 {
			statusClass = "warn"
		}
		b.WriteString(`<tr><td>`)
		b.WriteString(html.EscapeString(entry.Time.In(location).Format("2006-01-02 15:04:05")))
		b.WriteString(`</td><td class="`)
		b.WriteString(statusClass)
		b.WriteString(`">`)
		b.WriteString(strconv.Itoa(entry.Status))
		b.WriteString(`</td><td>`)
		b.WriteString(html.EscapeString(entry.Result))
		b.WriteString(`</td><td>`)
		b.WriteString(html.EscapeString(entry.Method))
		b.WriteString(`</td><td>`)
		b.WriteString(html.EscapeString(entry.TargetHost))
		b.WriteString(`</td><td class="url">`)
		b.WriteString(html.EscapeString(entry.Target))
		b.WriteString(`</td><td>`)
		b.WriteString(authLabel(entry))
		b.WriteString(`</td><td>`)
		b.WriteString(html.EscapeString(entry.ClientIP))
		b.WriteString(`</td><td>`)
		b.WriteString(html.EscapeString(entry.Duration.Truncate(time.Millisecond).String()))
		b.WriteString(`</td><td>`)
		b.WriteString(html.EscapeString(entry.Detail))
		b.WriteString(`</td></tr>`)
	}
	b.WriteString(`</tbody></table></div></main></body></html>`)
	return b.String()
}

func authLabel(entry proxyRequestLog) string {
	switch {
	case entry.AuthWhitelisted:
		return `<span class="pill">whitelist</span>`
	case entry.Authorized:
		return `<span class="pill">token</span>`
	default:
		return `<span class="pill">none</span>`
	}
}

func (p *Proxy) isAuthorized(token string) bool {
	// 未设置服务端 Token 时没有任何客户端凭据可以通过，避免意外形成开放代理。
	if p.authToken == "" {
		return false
	}
	expectedHash := sha256.Sum256([]byte(p.authToken))
	actualHash := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(expectedHash[:], actualHash[:]) == 1
}

func (p *Proxy) isAuthWhitelisted(targetURL *url.URL) bool {
	// 空白名单必须表示全部需要鉴权，不能复用域名白名单的“空即全放行”语义。
	return len(p.authWhitelist) > 0 && isDomainURLAllowed(targetURL, p.authWhitelist)
}

func proxyRaw(w http.ResponseWriter, resp *http.Response, req *http.Request, globalCORS bool) error {
	copyHeaders(resp.Header, w.Header())
	if globalCORS {
		setCORSHeaders(w)
	}
	if w.Header().Get("Referer") != "" {
		w.Header().Del("Referer")
		w.Header().Add("Referer", req.Host)
	}
	w.WriteHeader(resp.StatusCode)

	// Copy the response body to the output stream
	_, err := io.Copy(w, resp.Body)
	if err != nil {
		return err
	}
	return nil
}

func setCORSHeaders(w http.ResponseWriter) {
	clearCORSHeaders(w.Header())
	w.Header().Set("Access-Control-Allow-Origin", corsAllowOrigin)
	w.Header().Set("Access-Control-Allow-Methods", corsAllowMethods)
	w.Header().Set("Access-Control-Allow-Headers", corsAllowHeaders)
}

func clearCORSHeaders(header http.Header) {
	for k := range header {
		if strings.HasPrefix(strings.ToLower(k), "access-control-") {
			header.Del(k)
		}
	}
}

func proxyURL(r *http.Request) string {
	u := proxyURLPattern.ReplaceAllString(r.URL.Path, "$1//")
	if r.URL.RawQuery != "" {
		u += "?" + r.URL.RawQuery
	}
	return u
}

func parseTargetURL(rawURL string) (*url.URL, error) {
	targetURL, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	if targetURL.Host == "" || (targetURL.Scheme != "http" && targetURL.Scheme != "https") {
		return nil, fmt.Errorf("unsupported target url: %s", rawURL)
	}
	return targetURL, nil
}

func copyHeaders(src, dst http.Header) {
	for k, v := range src {
		for _, vv := range v {
			dst.Add(k, vv)
		}
	}
}

func disableUpstreamCompression(header http.Header) {
	header.Set("Accept-Encoding", identityEncoding)
}

func closeResponseBody(resp *http.Response) {
	if err := resp.Body.Close(); err != nil {
		log.Printf("Close response body error: %v", err)
	}
}

func newHTTPClient(socks5Proxy string) (*http.Client, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil

	if strings.TrimSpace(socks5Proxy) != "" {
		proxyURL, err := parseSocks5ProxyURL(socks5Proxy)
		if err != nil {
			return nil, err
		}
		transport.Proxy = http.ProxyURL(proxyURL)
	}

	return &http.Client{Transport: transport}, nil
}

func parseSocks5ProxyURL(rawProxy string) (*url.URL, error) {
	rawProxy = strings.TrimSpace(rawProxy)
	if rawProxy == "" {
		return nil, nil
	}
	if !strings.Contains(rawProxy, "://") {
		rawProxy = "socks5://" + rawProxy
	}

	proxyURL, err := url.Parse(rawProxy)
	if err != nil {
		return nil, fmt.Errorf("invalid socks5 proxy: %w", err)
	}
	proxyURL.Scheme = strings.ToLower(proxyURL.Scheme)
	if proxyURL.Scheme != "socks5" && proxyURL.Scheme != "socks5h" {
		return nil, fmt.Errorf("unsupported proxy scheme %q: only socks5 and socks5h are supported", proxyURL.Scheme)
	}
	if proxyURL.Host == "" {
		return nil, fmt.Errorf("invalid socks5 proxy: missing host")
	}
	return proxyURL, nil
}

func (p *Proxy) isDomainAllowed(targetURL *url.URL) bool {
	return isDomainURLAllowed(targetURL, p.domainWhitelist)
}

func (p *Proxy) checkDomain(targetURL *url.URL) error {
	if p.isDomainAllowed(targetURL) {
		return nil
	}
	return &domainNotAllowedError{host: targetURL.Hostname()}
}

func (p *Proxy) checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	if err := p.checkDomain(req.URL); err != nil {
		return err
	}
	authorized, _ := req.Context().Value(authorizationContextKey{}).(bool)
	if !authorized && !p.isAuthWhitelisted(req.URL) {
		return &authenticationRequiredError{}
	}
	return nil
}

type authorizationContextKey struct{}

type authenticationRequiredError struct{}

func (e *authenticationRequiredError) Error() string {
	return "authentication required after redirect"
}

type domainNotAllowedError struct {
	host string
}

func (e *domainNotAllowedError) Error() string {
	return "domain not allowed: " + e.host
}

type domainRule struct {
	exclude bool
	pattern string
	regexp  *regexp.Regexp
	port    string
}

func isDomainAllowed(host string, whitelist []domainRule) bool {
	target := normalizeTargetHostPort(host, "")
	return isDomainTargetAllowed(target, whitelist)
}

func isDomainURLAllowed(targetURL *url.URL, whitelist []domainRule) bool {
	target := normalizeTargetHostPort(targetURL.Host, targetURL.Scheme)
	return isDomainTargetAllowed(target, whitelist)
}

func isDomainTargetAllowed(target domainTarget, whitelist []domainRule) bool {
	if len(whitelist) == 0 {
		return true
	}

	allowed := false
	for _, rule := range whitelist {
		if !rule.matches(target) {
			continue
		}
		if rule.exclude {
			return false
		}
		allowed = true
	}
	return allowed
}

func normalizeDomainWhitelist(entries []string) []domainRule {
	whitelist := make([]domainRule, 0, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		rule, ok := normalizeDomainRule(entry)
		if !ok {
			continue
		}
		key := rule.key()
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		whitelist = append(whitelist, rule)
	}
	return whitelist
}

func normalizeDomainRule(entry string) (domainRule, bool) {
	entry = strings.TrimSpace(entry)
	exclude := strings.HasPrefix(entry, "-")
	if exclude {
		entry = strings.TrimSpace(strings.TrimPrefix(entry, "-"))
	}

	host, port := splitDomainRulePort(entry)
	host = normalizeDomain(host)
	if host == "" {
		return domainRule{}, false
	}

	rule := domainRule{exclude: exclude, pattern: host, port: port}
	if strings.ContainsAny(host, "*?") {
		rule.regexp = compileDomainWildcard(host)
	}
	return rule, true
}

func splitDomainRulePort(entry string) (string, string) {
	if host, port, err := net.SplitHostPort(entry); err == nil {
		return host, normalizePort(port)
	}
	if strings.Count(entry, ":") == 1 {
		idx := strings.LastIndex(entry, ":")
		port := normalizePort(entry[idx+1:])
		if port != "" {
			return entry[:idx], port
		}
	}
	return entry, ""
}

func normalizePort(port string) string {
	port = strings.TrimSpace(port)
	if port == "" {
		return ""
	}
	for _, r := range port {
		if r < '0' || r > '9' {
			return ""
		}
	}
	return port
}

func compileDomainWildcard(pattern string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString("^")
	for _, r := range pattern {
		switch r {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteString(".")
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	b.WriteString("$")
	return regexp.MustCompile(b.String())
}

func (r domainRule) key() string {
	if r.exclude {
		return "-" + r.pattern + ":" + r.port
	}
	return r.pattern + ":" + r.port
}

func (r domainRule) matches(target domainTarget) bool {
	if !r.matchesHost(target.host) {
		return false
	}
	return r.matchesPort(target.port)
}

func (r domainRule) matchesHost(host string) bool {
	if r.regexp != nil {
		return r.regexp.MatchString(host)
	}
	return host == r.pattern || strings.HasSuffix(host, "."+r.pattern)
}

func (r domainRule) matchesPort(port string) bool {
	if r.port == "0" {
		return true
	}
	if r.port != "" {
		return port == r.port
	}
	return port == "" || port == "80" || port == "443"
}

type domainTarget struct {
	host string
	port string
}

func normalizeTargetHostPort(rawHost, scheme string) domainTarget {
	host := rawHost
	port := ""
	if parsedHost, parsedPort, err := net.SplitHostPort(rawHost); err == nil {
		host = parsedHost
		port = parsedPort
	} else if strings.Count(rawHost, ":") == 1 {
		idx := strings.LastIndex(rawHost, ":")
		if parsedPort := normalizePort(rawHost[idx+1:]); parsedPort != "" {
			host = rawHost[:idx]
			port = parsedPort
		}
	}
	if port == "" {
		port = defaultPort(scheme)
	}
	return domainTarget{host: normalizeDomain(host), port: port}
}

func defaultPort(scheme string) string {
	switch strings.ToLower(scheme) {
	case "http":
		return "80"
	case "https":
		return "443"
	default:
		return ""
	}
}

func normalizeDomain(domain string) string {
	domain = strings.Trim(strings.ToLower(strings.TrimSpace(domain)), ".")
	domain = strings.TrimPrefix(strings.TrimSuffix(domain, "]"), "[")
	if domain == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(domain); err == nil {
		domain = strings.Trim(strings.ToLower(host), ".")
	}
	return domain
}
