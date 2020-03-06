package oauth2

import (
	"crypto/tls"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/18F/hmacauth"
	"github.com/QubitProducts/kube-ci/oauth2/cookie"
	"github.com/opentracing-contrib/go-stdlib/nethttp"
	opentracing "github.com/opentracing/opentracing-go"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type Provider interface {
	Data() *ProviderData
	GetEmailAddress(*SessionState) (string, error)
	Redeem(string, string) (*SessionState, error)
	ValidateGroup(string) bool
	ValidateSessionState(*SessionState) bool
	GetLoginURL(redirectURI, finalRedirect string) string
	RefreshSessionIfNeeded(*SessionState) (bool, error)
	SessionFromCookie(string, *cookie.Cipher) (*SessionState, error)
	CookieForSession(*SessionState, *cookie.Cipher) (string, error)
}

var (
	proxyVec     *prometheus.HistogramVec
	robotsVec    *prometheus.HistogramVec
	pingVec      *prometheus.HistogramVec
	whitelistVec *prometheus.HistogramVec
	signInVec    *prometheus.HistogramVec
	signOutVec   *prometheus.HistogramVec
	startVec     *prometheus.HistogramVec
	callbackVec  *prometheus.HistogramVec
	authOnlyVec  *prometheus.HistogramVec
)

func init() {
	histogramOpts := prometheus.HistogramOpts{
		Name:        "http_request_duration_seconds",
		Help:        "A histogram of latencies for requests.",
		Buckets:     []float64{.25, .5, 1, 2.5, 5, 10},
		ConstLabels: prometheus.Labels{"handler": "proxy"},
	}
	proxyVec = prometheus.NewHistogramVec(
		histogramOpts,
		[]string{"code"},
	)

	histogramOpts.ConstLabels = prometheus.Labels{"handler": "robots"}
	robotsVec = prometheus.NewHistogramVec(
		histogramOpts,
		[]string{"code"},
	)

	histogramOpts.ConstLabels = prometheus.Labels{"handler": "ping"}
	pingVec = prometheus.NewHistogramVec(
		histogramOpts,
		[]string{"code"},
	)

	histogramOpts.ConstLabels = prometheus.Labels{"handler": "whitelist"}
	whitelistVec = prometheus.NewHistogramVec(
		histogramOpts,
		[]string{"code"},
	)

	histogramOpts.ConstLabels = prometheus.Labels{"handler": "signIn"}
	signInVec = prometheus.NewHistogramVec(
		histogramOpts,
		[]string{"code"},
	)

	histogramOpts.ConstLabels = prometheus.Labels{"handler": "signOut"}
	signOutVec = prometheus.NewHistogramVec(
		histogramOpts,
		[]string{"code"},
	)

	histogramOpts.ConstLabels = prometheus.Labels{"handler": "start"}
	startVec = prometheus.NewHistogramVec(
		histogramOpts,
		[]string{"code"},
	)

	histogramOpts.ConstLabels = prometheus.Labels{"handler": "callback"}
	callbackVec = prometheus.NewHistogramVec(
		histogramOpts,
		[]string{"code"},
	)

	histogramOpts.ConstLabels = prometheus.Labels{"handler": "authOnly"}
	authOnlyVec = prometheus.NewHistogramVec(
		histogramOpts,
		[]string{"code"},
	)

	prometheus.MustRegister(
		proxyVec,
		robotsVec,
		pingVec,
		whitelistVec,
		signInVec,
		signOutVec,
		startVec,
		callbackVec,
		authOnlyVec,
	)
}

const SignatureHeader = "GAP-Signature"

var SignatureHeaders []string = []string{
	"Content-Length",
	"Content-Md5",
	"Content-Type",
	"Date",
	"Authorization",
	"X-Forwarded-User",
	"X-Forwarded-Email",
	"X-Forwarded-Access-Token",
	"Cookie",
	"Gap-Auth",
}

type OAuthProxy struct {
	CookieSeed     string
	CookieName     string
	CSRFCookieName string
	CookieDomain   string
	CookieSecure   bool
	CookieHttpOnly bool
	CookieExpire   time.Duration
	CookieRefresh  time.Duration
	Validator      func(string) bool

	RobotsPath        string
	MetricsPath       string
	PingPath          string
	OAuthStartPath    string
	OAuthCallbackPath string

	redirectURL        *url.URL // the url to receive requests at
	provider           Provider
	ProxyPrefix        string
	SignInMessage      string
	serveMux           http.Handler
	SetXAuthRequest    bool
	SkipProviderButton bool
	PassUserHeaders    bool
	PassAccessToken    bool
	CookieCipher       *cookie.Cipher
	skipAuthRegex      []string
	skipAuthPreflight  bool
	compiledRegex      []*regexp.Regexp
	templates          *template.Template
	Footer             string
	TLS                []tls.Certificate
}

type UpstreamProxy struct {
	upstream url.URL
	handler  http.Handler
	auth     hmacauth.HmacAuth
}

func (u *UpstreamProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("GAP-Upstream-Address", u.upstream.Host)
	if u.auth != nil {
		r.Header.Set("GAP-Auth", w.Header().Get("GAP-Auth"))
		u.auth.SignRequest(r)
	}
	if isWebsocketRequest(r) {
		u.handleWebsocket(w, r)
	} else {
		u.handler.ServeHTTP(w, r)
	}
}

type traceTransport struct{ next http.RoundTripper }

func (t traceTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	nt := &nethttp.Transport{t.next}
	r, ht := nethttp.TraceRequest(opentracing.GlobalTracer(), r)
	defer ht.Finish()
	return nt.RoundTrip(r)
}

func NewReverseProxy(target *url.URL) (proxy *httputil.ReverseProxy) {
	rp := httputil.NewSingleHostReverseProxy(target)
	rp.Transport = &traceTransport{rp.Transport}
	return rp
}

func setProxyUpstreamHostHeader(proxy *httputil.ReverseProxy, target *url.URL) {
	director := proxy.Director
	proxy.Director = func(req *http.Request) {
		director(req)
		// use RequestURI so that we aren't unescaping encoded slashes in the request path
		req.Host = target.Host
		req.URL.Opaque = req.RequestURI
		req.URL.RawQuery = ""
	}
}
func setProxyDirector(proxy *httputil.ReverseProxy) {
	director := proxy.Director
	proxy.Director = func(req *http.Request) {
		director(req)
		// use RequestURI so that we aren't unescaping encoded slashes in the request path
		req.URL.Opaque = req.RequestURI
		req.URL.RawQuery = ""
	}
}
func NewFileServer(path string, filesystemPath string) (proxy http.Handler) {
	return http.StripPrefix(path, http.FileServer(http.Dir(filesystemPath)))
}

func NewOAuthProxy(opts *Options, validator func(string) bool) *OAuthProxy {
	serveMux := http.NewServeMux()
	var auth hmacauth.HmacAuth
	if sigData := opts.signatureData; sigData != nil {
		auth = hmacauth.NewHmacAuth(sigData.hash, []byte(sigData.key),
			SignatureHeader, SignatureHeaders)
	}
	for _, u := range opts.proxyURLs {
		path := u.Path
		switch u.Scheme {
		case "http", "https":
			u.Path = ""
			log.Printf("mapping path %q => upstream %q", path, u)
			proxy := NewReverseProxy(u)
			if !opts.PassHostHeader {
				setProxyUpstreamHostHeader(proxy, u)
			} else {
				setProxyDirector(proxy)
			}
			serveMux.Handle(path,
				&UpstreamProxy{*u, proxy, auth})
		case "file":
			if u.Fragment != "" {
				path = u.Fragment
			}
			log.Printf("mapping path %q => file system %q", path, u.Path)
			proxy := NewFileServer(path, u.Path)
			serveMux.Handle(path, &UpstreamProxy{*u, proxy, nil})
		default:
			panic(fmt.Sprintf("unknown upstream protocol %s", u.Scheme))
		}
	}
	for _, u := range opts.CompiledRegex {
		log.Printf("compiled skip-auth-regex => %q", u)
	}

	redirectURL := opts.redirectURL
	redirectURL.Path = fmt.Sprintf("%s/callback", opts.ProxyPrefix)

	log.Printf("OAuthProxy configured for %s Client ID: %s", opts.provider.Data().ProviderName, opts.ClientID)
	domain := opts.CookieDomain
	if domain == "" {
		domain = "<default>"
	}
	refresh := "disabled"
	if opts.CookieRefresh != time.Duration(0) {
		refresh = fmt.Sprintf("after %s", opts.CookieRefresh)
	}

	log.Printf("Cookie settings: name:%s secure(https):%v httponly:%v expiry:%s domain:%s refresh:%s", opts.CookieName, opts.CookieSecure, opts.CookieHttpOnly, opts.CookieExpire, domain, refresh)

	var cipher *cookie.Cipher
	if opts.PassAccessToken || (opts.CookieRefresh != time.Duration(0)) {
		var err error
		cipher, err = cookie.NewCipher(secretBytes(opts.CookieSecret))
		if err != nil {
			log.Fatal("cookie-secret error: ", err)
		}
	}

	return &OAuthProxy{
		CookieName:     opts.CookieName,
		CSRFCookieName: fmt.Sprintf("%v_%v", opts.CookieName, "csrf"),
		CookieSeed:     opts.CookieSecret,
		CookieDomain:   opts.CookieDomain,
		CookieSecure:   opts.CookieSecure,
		CookieHttpOnly: opts.CookieHttpOnly,
		CookieExpire:   opts.CookieExpire,
		CookieRefresh:  opts.CookieRefresh,
		Validator:      validator,

		RobotsPath:        "/robots.txt",
		PingPath:          "/ping",
		MetricsPath:       fmt.Sprintf("%s/metrics", opts.ProxyPrefix),
		OAuthStartPath:    fmt.Sprintf("%s/start", opts.ProxyPrefix),
		OAuthCallbackPath: fmt.Sprintf("%s/callback", opts.ProxyPrefix),

		ProxyPrefix:        opts.ProxyPrefix,
		provider:           opts.provider,
		serveMux:           serveMux,
		redirectURL:        redirectURL,
		skipAuthRegex:      opts.SkipAuthRegex,
		skipAuthPreflight:  opts.SkipAuthPreflight,
		compiledRegex:      opts.CompiledRegex,
		SetXAuthRequest:    opts.SetXAuthRequest,
		PassUserHeaders:    opts.PassUserHeaders,
		PassAccessToken:    opts.PassAccessToken,
		SkipProviderButton: opts.SkipProviderButton,
		CookieCipher:       cipher,
		templates:          loadTemplates(),
		Footer:             opts.Footer,
	}
}

func (p *OAuthProxy) GetRedirectURI(host string) string {
	// default to the request Host if not set
	if p.redirectURL.Host != "" {
		return p.redirectURL.String()
	}
	var u url.URL
	u = *p.redirectURL
	if u.Scheme == "" {
		if p.CookieSecure {
			u.Scheme = "https"
		} else {
			u.Scheme = "http"
		}
	}
	u.Host = host
	return u.String()
}

func (p *OAuthProxy) redeemCode(host, code string) (s *SessionState, err error) {
	if code == "" {
		return nil, errors.New("missing code")
	}
	redirectURI := p.GetRedirectURI(host)
	s, err = p.provider.Redeem(redirectURI, code)
	if err != nil {
		return
	}

	if s.Email == "" {
		s.Email, err = p.provider.GetEmailAddress(s)
	}
	return
}

func (p *OAuthProxy) MakeSessionCookie(req *http.Request, value string, expiration time.Duration, now time.Time) *http.Cookie {
	if value != "" {
		value = cookie.SignedValue(p.CookieSeed, p.CookieName, value, now)
		if len(value) > 4096 {
			// Cookies cannot be larger than 4kb
			log.Printf("WARNING - Cookie Size: %d bytes", len(value))
		}
	}
	return p.makeCookie(req, p.CookieName, value, expiration, now)
}

func (p *OAuthProxy) MakeCSRFCookie(req *http.Request, value string, expiration time.Duration, now time.Time) *http.Cookie {
	return p.makeCookie(req, p.CSRFCookieName, value, expiration, now)
}

func (p *OAuthProxy) makeCookie(req *http.Request, name string, value string, expiration time.Duration, now time.Time) *http.Cookie {
	domain := req.Host
	if h, _, err := net.SplitHostPort(domain); err == nil {
		domain = h
	}
	if p.CookieDomain != "" {
		if !strings.HasSuffix(domain, p.CookieDomain) {
			log.Printf("Warning: request host is %q but using configured cookie domain of %q", domain, p.CookieDomain)
		}
		domain = p.CookieDomain
	}

	return &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		Domain:   domain,
		HttpOnly: p.CookieHttpOnly,
		Secure:   p.CookieSecure,
		Expires:  now.Add(expiration),
	}
}

func (p *OAuthProxy) ClearCSRFCookie(rw http.ResponseWriter, req *http.Request) {
	http.SetCookie(rw, p.MakeCSRFCookie(req, "", time.Hour*-1, time.Now()))
}

func (p *OAuthProxy) SetCSRFCookie(rw http.ResponseWriter, req *http.Request, val string) {
	http.SetCookie(rw, p.MakeCSRFCookie(req, val, p.CookieExpire, time.Now()))
}

func (p *OAuthProxy) ClearSessionCookie(rw http.ResponseWriter, req *http.Request) {
	http.SetCookie(rw, p.MakeSessionCookie(req, "", time.Hour*-1, time.Now()))
}

func (p *OAuthProxy) SetSessionCookie(rw http.ResponseWriter, req *http.Request, val string) {
	http.SetCookie(rw, p.MakeSessionCookie(req, val, p.CookieExpire, time.Now()))
}

func (p *OAuthProxy) LoadCookiedSession(req *http.Request) (*SessionState, time.Duration, error) {
	var age time.Duration
	c, err := req.Cookie(p.CookieName)
	if err != nil {
		// always http.ErrNoCookie
		return nil, age, fmt.Errorf("Cookie %q not present", p.CookieName)
	}
	val, timestamp, ok := cookie.Validate(c, p.CookieSeed, p.CookieExpire)
	if !ok {
		return nil, age, errors.New("Cookie Signature not valid")
	}

	session, err := p.provider.SessionFromCookie(val, p.CookieCipher)
	if err != nil {
		return nil, age, err
	}

	age = time.Now().Truncate(time.Second).Sub(timestamp)
	return session, age, nil
}

func (p *OAuthProxy) SaveSession(rw http.ResponseWriter, req *http.Request, s *SessionState) error {
	value, err := p.provider.CookieForSession(s, p.CookieCipher)
	if err != nil {
		return err
	}
	p.SetSessionCookie(rw, req, value)
	return nil
}

func (p *OAuthProxy) RobotsTxt(rw http.ResponseWriter) {
	rw.WriteHeader(http.StatusOK)
	fmt.Fprintf(rw, "User-agent: *\nDisallow: /")
}

func (p *OAuthProxy) PingPage(rw http.ResponseWriter) {
	rw.WriteHeader(http.StatusOK)
	fmt.Fprintf(rw, "OK")
}

func (p *OAuthProxy) ErrorPage(rw http.ResponseWriter, code int, title string, message string) {
	log.Printf("ErrorPage %d %s %s", code, title, message)
	rw.WriteHeader(code)
	t := struct {
		Title       string
		Message     string
		ProxyPrefix string
	}{
		Title:       fmt.Sprintf("%d %s", code, title),
		Message:     message,
		ProxyPrefix: p.ProxyPrefix,
	}
	p.templates.ExecuteTemplate(rw, "error.html", t)
}

func (p *OAuthProxy) GetRedirect(req *http.Request) (redirect string, err error) {
	err = req.ParseForm()
	if err != nil {
		return
	}

	redirect = req.Form.Get("rd")
	if redirect == "" || !strings.HasPrefix(redirect, "/") || strings.HasPrefix(redirect, "//") {
		redirect = "/"
	}

	return
}

func (p *OAuthProxy) IsWhitelistedRequest(req *http.Request) (ok bool) {
	isPreflightRequestAllowed := p.skipAuthPreflight && req.Method == "OPTIONS"
	return isPreflightRequestAllowed || p.IsWhitelistedPath(req.URL.Path)
}

func (p *OAuthProxy) IsWhitelistedPath(path string) (ok bool) {
	for _, u := range p.compiledRegex {
		ok = u.MatchString(path)
		if ok {
			return
		}
	}
	return
}

func getRemoteAddr(req *http.Request) (s string) {
	s = req.RemoteAddr
	if req.Header.Get("X-Real-IP") != "" {
		s += fmt.Sprintf(" (%q)", req.Header.Get("X-Real-IP"))
	}
	return
}

func instrument(next http.HandlerFunc, dvec *prometheus.HistogramVec, spanName string) http.Handler {
	return promhttp.InstrumentHandlerDuration(dvec, next)
}

func (p *OAuthProxy) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	switch path := req.URL.Path; {
	case path == p.RobotsPath:
		instrument(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
			p.RobotsTxt(rw)
		}), robotsVec, "robots").ServeHTTP(rw, req)
	case path == p.MetricsPath:
		promhttp.Handler().ServeHTTP(rw, req)
	case path == p.PingPath:
		instrument(func(rw http.ResponseWriter, req *http.Request) {
			p.PingPage(rw)
		}, pingVec, "ping").ServeHTTP(rw, req)
	case p.IsWhitelistedRequest(req):
		instrument(p.serveMux.ServeHTTP, whitelistVec, "whitelist").ServeHTTP(rw, req)
	case path == p.OAuthStartPath:
		instrument(p.OAuthStart, startVec, "start").ServeHTTP(rw, req)
	case path == p.OAuthCallbackPath:
		instrument(p.OAuthCallback, callbackVec, "callback").ServeHTTP(rw, req)
	default:
		instrument(p.Proxy, proxyVec, "proxy").ServeHTTP(rw, req)
	}
}

func (p *OAuthProxy) OAuthStart(rw http.ResponseWriter, req *http.Request) {
	nonce, err := cookie.Nonce()
	if err != nil {
		p.ErrorPage(rw, 500, "Internal Error", err.Error())
		return
	}
	p.SetCSRFCookie(rw, req, nonce)
	redirect, err := p.GetRedirect(req)
	if err != nil {
		p.ErrorPage(rw, 500, "Internal Error", err.Error())
		return
	}
	redirectURI := p.GetRedirectURI(req.Host)
	http.Redirect(rw, req, p.provider.GetLoginURL(redirectURI, fmt.Sprintf("%v:%v", nonce, redirect)), 302)
}

func (p *OAuthProxy) OAuthCallback(rw http.ResponseWriter, req *http.Request) {
	remoteAddr := getRemoteAddr(req)

	// finish the oauth cycle
	err := req.ParseForm()
	if err != nil {
		p.ErrorPage(rw, 500, "Internal Error", err.Error())
		return
	}
	errorString := req.Form.Get("error")
	if errorString != "" {
		p.ErrorPage(rw, 403, "Permission Denied", errorString)
		return
	}

	session, err := p.redeemCode(req.Host, req.Form.Get("token"))
	if err != nil {
		log.Printf("%s error redeeming code %s", remoteAddr, err)
		p.ErrorPage(rw, 500, "Internal Error", "Internal Error")
		return
	}

	s := strings.SplitN(req.Form.Get("state"), ":", 2)
	if len(s) != 2 {
		p.ErrorPage(rw, 500, "Internal Error", "Invalid State")
		return
	}
	nonce := s[0]
	redirect := s[1]
	c, err := req.Cookie(p.CSRFCookieName)
	if err != nil {
		p.ErrorPage(rw, 403, "Permission Denied", err.Error())
		return
	}
	p.ClearCSRFCookie(rw, req)
	if c.Value != nonce {
		log.Printf("%s csrf token mismatch, potential attack", remoteAddr)
		p.ErrorPage(rw, 403, "Permission Denied", "csrf failed")
		return
	}

	if !strings.HasPrefix(redirect, "/") || strings.HasPrefix(redirect, "//") {
		redirect = "/"
	}

	// set cookie, or deny
	if p.Validator(session.Email) && p.provider.ValidateGroup(session.Email) {
		log.Printf("%s authentication complete %s", remoteAddr, session)
		err := p.SaveSession(rw, req, session)
		if err != nil {
			log.Printf("%s %s", remoteAddr, err)
			p.ErrorPage(rw, 500, "Internal Error", "Internal Error")
			return
		}
		http.Redirect(rw, req, redirect, 302)
	} else {
		log.Printf("%s Permission Denied: %q is unauthorized", remoteAddr, session.Email)
		p.ErrorPage(rw, 403, "Permission Denied", "Invalid Account")
	}
}

func (p *OAuthProxy) Proxy(rw http.ResponseWriter, req *http.Request) {
	status := p.Authenticate(rw, req)
	if status == http.StatusInternalServerError {
		p.ErrorPage(rw, http.StatusInternalServerError,
			"Internal Error", "Internal Error")
	} else if status == http.StatusForbidden {
		p.OAuthStart(rw, req)
	} else {
		p.serveMux.ServeHTTP(rw, req)
	}
}

func (p *OAuthProxy) Authenticate(rw http.ResponseWriter, req *http.Request) int {
	var saveSession, clearSession, revalidated bool
	remoteAddr := getRemoteAddr(req)

	session, sessionAge, err := p.LoadCookiedSession(req)
	if err != nil {
		log.Printf("%s %s", remoteAddr, err)
	}
	if session != nil && sessionAge > p.CookieRefresh && p.CookieRefresh != time.Duration(0) {
		log.Printf("%s refreshing %s old session cookie for %s (refresh after %s)", remoteAddr, sessionAge, session, p.CookieRefresh)
		saveSession = true
	}

	if ok, err := p.provider.RefreshSessionIfNeeded(session); err != nil {
		log.Printf("%s removing session. error refreshing access token %s %s", remoteAddr, err, session)
		clearSession = true
		session = nil
	} else if ok {
		saveSession = true
		revalidated = true
	}

	if session != nil && session.IsExpired() {
		log.Printf("%s removing session. token expired %s", remoteAddr, session)
		session = nil
		saveSession = false
		clearSession = true
	}

	if saveSession && !revalidated && session != nil && session.AccessToken != "" {
		if !p.provider.ValidateSessionState(session) {
			log.Printf("%s removing session. error validating %s", remoteAddr, session)
			saveSession = false
			session = nil
			clearSession = true
		}
	}

	if session != nil && session.Email != "" && !p.Validator(session.Email) {
		log.Printf("%s Permission Denied: removing session %s", remoteAddr, session)
		session = nil
		saveSession = false
		clearSession = true
	}

	if saveSession && session != nil {
		err := p.SaveSession(rw, req, session)
		if err != nil {
			log.Printf("%s %s", remoteAddr, err)
			return http.StatusInternalServerError
		}
	}

	if clearSession {
		p.ClearSessionCookie(rw, req)
	}

	if session == nil {
		return http.StatusForbidden
	}

	// At this point, the user is authenticated. proxy normally
	if p.PassUserHeaders {
		req.Header["X-Forwarded-User"] = []string{session.User}
		if session.Email != "" {
			req.Header["X-Forwarded-Email"] = []string{session.Email}
		}
	}
	if p.SetXAuthRequest {
		rw.Header().Set("X-Auth-Request-User", session.User)
		if session.Email != "" {
			rw.Header().Set("X-Auth-Request-Email", session.Email)
		}
	}
	if p.PassAccessToken && session.AccessToken != "" {
		req.Header["X-Forwarded-Access-Token"] = []string{session.AccessToken}
	}
	if session.Email == "" {
		rw.Header().Set("GAP-Auth", session.User)
	} else {
		rw.Header().Set("GAP-Auth", session.Email)
	}
	return http.StatusAccepted
}
