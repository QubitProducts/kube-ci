package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/bitly/go-simplejson"
	"github.com/pkg/errors"
)

// stripToken is a helper function to obfuscate "access_token"
// query parameters
func stripToken(endpoint string) string {
	return stripParam("access_token", endpoint)
}

// stripParam generalizes the obfuscation of a particular
// query parameter - typically 'access_token' or 'client_secret'
// The parameter's second half is replaced by '...' and returned
// as part of the encoded query parameters.
// If the target parameter isn't found, the endpoint is returned
// unmodified.
func stripParam(param, endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil {
		log.Printf("error attempting to strip %s: %s", param, err)
		return endpoint
	}

	if u.RawQuery != "" {
		values, err := url.ParseQuery(u.RawQuery)
		if err != nil {
			log.Printf("error attempting to strip %s: %s", param, err)
			return u.String()
		}

		if val := values.Get(param); val != "" {
			values.Set(param, val[:(len(val)/2)]+"...")
			u.RawQuery = values.Encode()
			return u.String()
		}
	}

	return endpoint
}

func Request(req *http.Request) (*simplejson.Json, error) {
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("%s %s %s", req.Method, req.URL, err)
		return nil, err
	}
	body, err := ioutil.ReadAll(resp.Body)
	resp.Body.Close()
	log.Printf("%d %s %s %s", resp.StatusCode, req.Method, req.URL, body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("got %d %s", resp.StatusCode, body)
	}
	data, err := simplejson.NewJson(body)
	if err != nil {
		return nil, err
	}
	return data, nil
}

func RequestJson(req *http.Request, v interface{}) error {
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("%s %s %s", req.Method, req.URL, err)
		return err
	}
	body, err := ioutil.ReadAll(resp.Body)
	resp.Body.Close()
	log.Printf("%d %s %s %s", resp.StatusCode, req.Method, req.URL, body)
	if err != nil {
		return err
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("got %d %s", resp.StatusCode, body)
	}
	return json.Unmarshal(body, v)
}

func RequestUnparsedResponse(url string) (resp *http.Response, err error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}

	return http.DefaultClient.Do(req)
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
	SignInPath        string
	SignOutPath       string
	OAuthStartPath    string
	OAuthCallbackPath string
	AuthOnlyPath      string

	redirectURL         *url.URL // the url to receive requests at
	ProxyPrefix         string
	SignInMessage       string
	DisplayHtpasswdForm bool

	SetXAuthRequest    bool
	PassBasicAuth      bool
	SkipProviderButton bool
	PassUserHeaders    bool
	BasicAuthPassword  string
	PassAccessToken    bool
	CookieCipher       *Cipher
	Footer             string
	ClientID           string
	ClientSecret       string
	LoginURL           *url.URL
	RedeemURL          *url.URL
	ProfileURL         *url.URL
	ProtectedResource  *url.URL
	ValidateURL        *url.URL
	Scope              string

	Org  string
	Team string

	serveMux *http.ServeMux
}

var (
	host                 = "github.com"
	loginPath     string = "/login/oauth/authorize"
	redeemPath    string = "/login/oauth/access_token"
	validateHost  string = "api.github.com"
	scope         string = "read:org"
	cookieRefresh time.Duration
)

func (p *OAuthProxy) GetRedirect(req *http.Request) (redirect string, err error) {
	err = req.ParseForm()
	if err != nil {
		return
	}

	if !p.SkipProviderButton {
		redirect = req.Form.Get("rd")
	} else {
		redirect = req.URL.RequestURI()
		if req.Header.Get("X-Auth-Request-Redirect") != "" {
			redirect = req.Header.Get("X-Auth-Request-Redirect")
		}
	}
	if redirect == "" || !strings.HasPrefix(redirect, "/") || strings.HasPrefix(redirect, "//") {
		redirect = "/"
	}

	return
}

func (p *OAuthProxy) Redeem(redirectURL, code string) (s *SessionState, err error) {
	if code == "" {
		err = errors.New("missing code")
		return
	}

	params := url.Values{}
	params.Add("redirect_uri", redirectURL)
	params.Add("client_id", p.ClientID)
	params.Add("client_secret", p.ClientSecret)
	params.Add("code", code)
	params.Add("grant_type", "authorization_code")
	if p.ProtectedResource != nil && p.ProtectedResource.String() != "" {
		params.Add("resource", p.ProtectedResource.String())
	}

	var req *http.Request
	req, err = http.NewRequest("POST", p.RedeemURL.String(), bytes.NewBufferString(params.Encode()))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	var resp *http.Response
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	var body []byte
	body, err = ioutil.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return
	}

	if resp.StatusCode != 200 {
		err = fmt.Errorf("got %d from %q %s", resp.StatusCode, p.RedeemURL.String(), body)
		return
	}

	// blindly try json and x-www-form-urlencoded
	var jsonResponse struct {
		AccessToken string `json:"access_token"`
	}
	err = json.Unmarshal(body, &jsonResponse)
	if err == nil {
		s = &SessionState{
			AccessToken: jsonResponse.AccessToken,
		}
		return
	}

	var v url.Values
	v, err = url.ParseQuery(string(body))
	if err != nil {
		return
	}
	if a := v.Get("access_token"); a != "" {
		s = &SessionState{AccessToken: a}
	} else {
		err = fmt.Errorf("no access token found %s", body)
	}
	return
}

// GetLoginURL with typical oauth parameters
func (p *OAuthProxy) GetLoginURL(redirectURI, state string) string {
	var a url.URL
	a = *p.LoginURL
	params, _ := url.ParseQuery(a.RawQuery)
	params.Set("redirect_uri", redirectURI)
	params.Set("approval_prompt", `force`)
	params.Add("scope", p.Scope)
	params.Set("client_id", p.ClientID)
	params.Set("response_type", "code")
	params.Add("state", state)
	a.RawQuery = params.Encode()
	return a.String()
}

// CookieForSession serializes a session state for storage in a cookie
func (p *OAuthProxy) CookieForSession(s *SessionState, c *Cipher) (string, error) {
	return s.EncodeSessionState(c)
}

// SessionFromCookie deserializes a session from a cookie value
func (p *OAuthProxy) SessionFromCookie(v string, c *Cipher) (s *SessionState, err error) {
	return DecodeSessionState(v, c)
}

// ValidateGroup validates that the provided email exists in the configured provider
// email group(s).
func (p *OAuthProxy) ValidateGroup(email string) bool {
	return true
}

func (p *OAuthProxy) ValidateSessionState(s *SessionState) bool {
	return p.validateToken(s.AccessToken)
}

func getRemoteAddr(req *http.Request) (s string) {
	s = req.RemoteAddr
	if req.Header.Get("X-Real-IP") != "" {
		s += fmt.Sprintf(" (%q)", req.Header.Get("X-Real-IP"))
	}
	return
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
	s, err = p.Redeem(redirectURI, code)
	if err != nil {
		return
	}

	if s.Email == "" {
		s.Email, err = p.GetEmailAddress(s)
	}
	return
}

func (p *OAuthProxy) MakeSessionCookie(req *http.Request, value string, expiration time.Duration, now time.Time) *http.Cookie {
	if value != "" {
		value = SignedValue(p.CookieSeed, p.CookieName, value, now)
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

// validateToken returns true if token is valid
func (p *OAuthProxy) validateToken(access_token string) bool {
	if access_token == "" || p.ValidateURL == nil {
		return false
	}
	endpoint := p.ValidateURL.String()
	params := url.Values{"access_token": {access_token}}
	endpoint = endpoint + "?" + params.Encode()

	resp, err := RequestUnparsedResponse(endpoint)
	if err != nil {
		log.Printf("GET %s", endpoint)
		log.Printf("token validation request failed: %s", err)
		return false
	}

	body, _ := ioutil.ReadAll(resp.Body)
	resp.Body.Close()
	log.Printf("%d GET %s %s", resp.StatusCode, stripToken(endpoint), body)

	if resp.StatusCode == 200 {
		return true
	}
	log.Printf("token validation request failed: status %d - %s", resp.StatusCode, body)
	return false
}

func (p *OAuthProxy) hasOrg(accessToken string) (bool, error) {
	// https://developer.github.com/v3/orgs/#list-your-organizations

	var orgs []struct {
		Login string `json:"login"`
	}

	params := url.Values{
		"limit": {"100"},
	}

	endpoint := &url.URL{
		Scheme:   "https",
		Host:     validateHost,
		Path:     "/user/orgs",
		RawQuery: params.Encode(),
	}

	req, _ := http.NewRequest("GET", endpoint.String(), nil)
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	req.Header.Set("Authorization", fmt.Sprintf("token %s", accessToken))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, err
	}

	body, err := ioutil.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return false, err
	}
	if resp.StatusCode != 200 {
		return false, fmt.Errorf(
			"got %d from %q %s", resp.StatusCode, endpoint.String(), body)
	}

	if err := json.Unmarshal(body, &orgs); err != nil {
		return false, err
	}

	var presentOrgs []string
	for _, org := range orgs {
		if p.Org == org.Login {
			log.Printf("Found Github Organization: %q", org.Login)
			return true, nil
		}
		presentOrgs = append(presentOrgs, org.Login)
	}

	log.Printf("Missing Organization:%q in %v", p.Org, presentOrgs)
	return false, nil
}

func (p *OAuthProxy) hasOrgAndTeam(accessToken string) (bool, error) {
	// https://developer.github.com/v3/orgs/teams/#list-user-teams

	var teams []struct {
		Name string `json:"name"`
		Slug string `json:"slug"`
		Org  struct {
			Login string `json:"login"`
		} `json:"organization"`
	}

	params := url.Values{
		"limit": {"100"},
	}

	endpoint := &url.URL{
		Scheme:   "https",
		Host:     validateHost,
		Path:     "/user/teams",
		RawQuery: params.Encode(),
	}
	req, _ := http.NewRequest("GET", endpoint.String(), nil)
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	req.Header.Set("Authorization", fmt.Sprintf("token %s", accessToken))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, err
	}

	body, err := ioutil.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return false, err
	}
	if resp.StatusCode != 200 {
		return false, fmt.Errorf(
			"got %d from %q %s", resp.StatusCode, endpoint.String(), body)
	}

	if err := json.Unmarshal(body, &teams); err != nil {
		return false, fmt.Errorf("%s unmarshaling %s", err, body)
	}

	var hasOrg bool
	presentOrgs := make(map[string]bool)
	var presentTeams []string
	for _, team := range teams {
		presentOrgs[team.Org.Login] = true
		if p.Org == team.Org.Login {
			hasOrg = true
			ts := strings.Split(p.Team, ",")
			for _, t := range ts {
				if t == team.Slug {
					log.Printf("Found Github Organization:%q Team:%q (Name:%q)", team.Org.Login, team.Slug, team.Name)
					return true, nil
				}
			}
			presentTeams = append(presentTeams, team.Slug)
		}
	}
	if hasOrg {
		log.Printf("Missing Team:%q from Org:%q in teams: %v", p.Team, p.Org, presentTeams)
	} else {
		var allOrgs []string
		for org, _ := range presentOrgs {
			allOrgs = append(allOrgs, org)
		}
		log.Printf("Missing Organization:%q in %#v", p.Org, allOrgs)
	}
	return false, nil
}

func (p *OAuthProxy) GetEmailAddress(s *SessionState) (string, error) {
	var emails []struct {
		Email   string `json:"email"`
		Primary bool   `json:"primary"`
	}

	// if we require an Org or Team, check that first
	if p.Org != "" {
		if p.Team != "" {
			if ok, err := p.hasOrgAndTeam(s.AccessToken); err != nil || !ok {
				return "", err
			}
		} else {
			if ok, err := p.hasOrg(s.AccessToken); err != nil || !ok {
				return "", err
			}
		}
	}

	endpoint := &url.URL{
		Scheme: "https",
		Host:   validateHost,
		Path:   "/user/emails",
	}
	req, _ := http.NewRequest("GET", endpoint.String(), nil)
	req.Header.Set("Authorization", fmt.Sprintf("token %s", s.AccessToken))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	body, err := ioutil.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return "", err
	}

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("got %d from %q %s",
			resp.StatusCode, endpoint.String(), body)
	} else {
		log.Printf("got %d from %q %s", resp.StatusCode, endpoint.String(), body)
	}

	if err := json.Unmarshal(body, &emails); err != nil {
		return "", fmt.Errorf("%s unmarshaling %s", err, body)
	}

	for _, email := range emails {
		if email.Primary {
			return email.Email, nil
		}
	}

	return "", nil
}

func (p *OAuthProxy) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	status := p.Authenticate(w, req)
	if status == http.StatusInternalServerError {
		http.Error(w, http.StatusText(status), status)
	} else if status == http.StatusForbidden {
		p.OAuthStart(w, req)
	} else {
		p.serveMux.ServeHTTP(w, req)
	}
}

func (p *OAuthProxy) Authenticate(rw http.ResponseWriter, req *http.Request) int {
	if req.TLS != nil && len(req.TLS.PeerCertificates) > 0 {
		return http.StatusAccepted
	}

	var saveSession, clearSession, revalidated bool
	remoteAddr := getRemoteAddr(req)

	session, sessionAge, err := p.LoadCookiedSession(req)
	if err != nil {
		log.Printf("%s %s", remoteAddr, err)
	}

	if session != nil && sessionAge > cookieRefresh && cookieRefresh != time.Duration(0) {
		log.Printf("%s refreshing %s old session cookie for %s (refresh after %s)", remoteAddr, sessionAge, session, cookieRefresh)
		saveSession = true
	}

	if session != nil && session.IsExpired() {
		log.Printf("%s removing session. token expired %s", remoteAddr, session)
		session = nil
		saveSession = false
		clearSession = true
	}

	if saveSession && !revalidated && session != nil && session.AccessToken != "" {
		if !p.validateToken(session.AccessToken) {
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
	req.Header["X-Forwarded-User"] = []string{session.User}
	if session.Email != "" {
		req.Header["X-Forwarded-Email"] = []string{session.Email}
	}
	rw.Header().Set("X-Auth-Request-User", session.User)
	if session.Email != "" {
		rw.Header().Set("X-Auth-Request-Email", session.Email)
	}

	req.Header["X-Forwarded-Access-Token"] = []string{session.AccessToken}

	return http.StatusAccepted
}

func Nonce() (nonce string, err error) {
	b := make([]byte, 16)
	_, err = rand.Read(b)
	if err != nil {
		return
	}
	nonce = fmt.Sprintf("%x", b)
	return
}

// cookies are stored in a 3 part (value + timestamp + signature) to enforce that the values are as originally set.
// additionally, the 'value' is encrypted so it's opaque to the browser
// Validate ensures a cookie is properly signed
func Validate(cookie *http.Cookie, seed string, expiration time.Duration) (value string, t time.Time, ok bool) {
	// value, timestamp, sig
	parts := strings.Split(cookie.Value, "|")
	if len(parts) != 3 {
		return
	}
	sig := cookieSignature(seed, cookie.Name, parts[0], parts[1])
	if checkHmac(parts[2], sig) {
		ts, err := strconv.Atoi(parts[1])
		if err != nil {
			return
		}
		// The expiration timestamp set when the cookie was created
		// isn't sent back by the browser. Hence, we check whether the
		// creation timestamp stored in the cookie falls within the
		// window defined by (Now()-expiration, Now()].
		t = time.Unix(int64(ts), 0)
		if t.After(time.Now().Add(expiration*-1)) && t.Before(time.Now().Add(time.Minute*5)) {
			// it's a valid cookie. now get the contents
			rawValue, err := base64.URLEncoding.DecodeString(parts[0])
			if err == nil {
				value = string(rawValue)
				ok = true
				return
			}
		}
	}
	return
}

// SignedValue returns a cookie that is signed and can later be checked with Validate
func SignedValue(seed string, key string, value string, now time.Time) string {
	encodedValue := base64.URLEncoding.EncodeToString([]byte(value))
	timeStr := fmt.Sprintf("%d", now.Unix())
	sig := cookieSignature(seed, key, encodedValue, timeStr)
	cookieVal := fmt.Sprintf("%s|%s|%s", encodedValue, timeStr, sig)
	return cookieVal
}

func cookieSignature(args ...string) string {
	h := hmac.New(sha1.New, []byte(args[0]))
	for _, arg := range args[1:] {
		h.Write([]byte(arg))
	}
	var b []byte
	b = h.Sum(b)
	return base64.URLEncoding.EncodeToString(b)
}

func checkHmac(input, expected string) bool {
	inputMAC, err1 := base64.URLEncoding.DecodeString(input)
	if err1 == nil {
		expectedMAC, err2 := base64.URLEncoding.DecodeString(expected)
		if err2 == nil {
			return hmac.Equal(inputMAC, expectedMAC)
		}
	}
	return false
}

// Cipher provides methods to encrypt and decrypt cookie values
type Cipher struct {
	cipher.Block
}

// NewCipher returns a new aes Cipher for encrypting cookie values
func NewCipher(secret []byte) (*Cipher, error) {
	c, err := aes.NewCipher(secret)
	if err != nil {
		return nil, err
	}
	return &Cipher{Block: c}, err
}

// Encrypt a value for use in a cookie
func (c *Cipher) Encrypt(value string) (string, error) {
	ciphertext := make([]byte, aes.BlockSize+len(value))
	iv := ciphertext[:aes.BlockSize]
	if _, err := io.ReadFull(rand.Reader, iv); err != nil {
		return "", fmt.Errorf("failed to create initialization vector %s", err)
	}

	stream := cipher.NewCFBEncrypter(c.Block, iv)
	stream.XORKeyStream(ciphertext[aes.BlockSize:], []byte(value))
	return base64.StdEncoding.EncodeToString(ciphertext), nil
}

// Decrypt a value from a cookie to it's original string
func (c *Cipher) Decrypt(s string) (string, error) {
	encrypted, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return "", fmt.Errorf("failed to decrypt cookie value %s", err)
	}

	if len(encrypted) < aes.BlockSize {
		return "", fmt.Errorf("encrypted cookie value should be "+
			"at least %d bytes, but is only %d bytes",
			aes.BlockSize, len(encrypted))
	}

	iv := encrypted[:aes.BlockSize]
	encrypted = encrypted[aes.BlockSize:]
	stream := cipher.NewCFBDecrypter(c.Block, iv)
	stream.XORKeyStream(encrypted, encrypted)

	return string(encrypted), nil
}

type SessionState struct {
	AccessToken  string
	ExpiresOn    time.Time
	RefreshToken string
	Email        string
	User         string
}

func (s *SessionState) IsExpired() bool {
	if !s.ExpiresOn.IsZero() && s.ExpiresOn.Before(time.Now()) {
		return true
	}
	return false
}

func (s *SessionState) String() string {
	o := fmt.Sprintf("Session{%s", s.userOrEmail())
	if s.AccessToken != "" {
		o += " token:true"
	}
	if !s.ExpiresOn.IsZero() {
		o += fmt.Sprintf(" expires:%s", s.ExpiresOn)
	}
	if s.RefreshToken != "" {
		o += " refresh_token:true"
	}
	return o + "}"
}

func (s *SessionState) EncodeSessionState(c *Cipher) (string, error) {
	if c == nil || s.AccessToken == "" {
		return s.userOrEmail(), nil
	}
	return s.EncryptedString(c)
}

func (s *SessionState) userOrEmail() string {
	u := s.User
	if s.Email != "" {
		u = s.Email
	}
	return u
}

func (s *SessionState) EncryptedString(c *Cipher) (string, error) {
	var err error
	if c == nil {
		panic("error. missing cipher")
	}
	a := s.AccessToken
	if a != "" {
		a, err = c.Encrypt(a)
		if err != nil {
			return "", err
		}
	}
	r := s.RefreshToken
	if r != "" {
		r, err = c.Encrypt(r)
		if err != nil {
			return "", err
		}
	}
	return fmt.Sprintf("%s|%s|%d|%s", s.userOrEmail(), a, s.ExpiresOn.Unix(), r), nil
}

func DecodeSessionState(v string, c *Cipher) (s *SessionState, err error) {
	chunks := strings.Split(v, "|")
	if len(chunks) == 1 {
		if strings.Contains(chunks[0], "@") {
			u := strings.Split(v, "@")[0]
			return &SessionState{Email: v, User: u}, nil
		}
		return &SessionState{User: v}, nil
	}

	if len(chunks) != 4 {
		err = fmt.Errorf("invalid number of fields (got %d expected 4)", len(chunks))
		return
	}

	s = &SessionState{}
	if c != nil && chunks[1] != "" {
		s.AccessToken, err = c.Decrypt(chunks[1])
		if err != nil {
			return nil, err
		}
	}
	if c != nil && chunks[3] != "" {
		s.RefreshToken, err = c.Decrypt(chunks[3])
		if err != nil {
			return nil, err
		}
	}
	if u := chunks[0]; strings.Contains(u, "@") {
		s.Email = u
		s.User = strings.Split(u, "@")[0]
	} else {
		s.User = u
	}
	ts, _ := strconv.Atoi(chunks[2])
	s.ExpiresOn = time.Unix(int64(ts), 0)
	return
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
	val, timestamp, ok := Validate(c, p.CookieSeed, p.CookieExpire)
	if !ok {
		return nil, age, errors.New("Cookie Signature not valid")
	}

	session, err := p.SessionFromCookie(val, p.CookieCipher)
	if err != nil {
		return nil, age, err
	}

	age = time.Now().Truncate(time.Second).Sub(timestamp)
	return session, age, nil
}

func (p *OAuthProxy) SaveSession(rw http.ResponseWriter, req *http.Request, s *SessionState) error {
	value, err := p.CookieForSession(s, p.CookieCipher)
	if err != nil {
		return err
	}
	p.SetSessionCookie(rw, req, value)
	return nil
}

func (p *OAuthProxy) OAuthStart(rw http.ResponseWriter, req *http.Request) {
	nonce, err := Nonce()
	if err != nil {
		http.Error(rw, "Internal Error", 500)
		return
	}
	p.SetCSRFCookie(rw, req, nonce)
	redirect, err := p.GetRedirect(req)
	if err != nil {
		http.Error(rw, "Internal Error", 500)
		return
	}
	redirectURI := p.GetRedirectURI(req.Host)
	http.Redirect(rw, req, p.GetLoginURL(redirectURI, fmt.Sprintf("%v:%v", nonce, redirect)), 302)
}

func (p *OAuthProxy) OAuthCallback(rw http.ResponseWriter, req *http.Request) {
	remoteAddr := getRemoteAddr(req)

	// finish the oauth cycle
	err := req.ParseForm()
	if err != nil {
		http.Error(rw, "Internal Error", 500)
		return
	}
	errorString := req.Form.Get("error")
	if errorString != "" {
		http.Error(rw, "Permission Denied", 403)
		return
	}

	session, err := p.redeemCode(req.Host, req.Form.Get("token"))
	if err != nil {
		log.Printf("%s error redeeming code %s", remoteAddr, err)
		http.Error(rw, "Internal Error", 500)
		return
	}

	s := strings.SplitN(req.Form.Get("state"), ":", 2)
	if len(s) != 2 {
		http.Error(rw, "Internal Error", 500)
		return
	}
	nonce := s[0]
	redirect := s[1]
	c, err := req.Cookie(p.CSRFCookieName)
	if err != nil {
		http.Error(rw, "Permission Denied", 403)
		return
	}
	p.ClearCSRFCookie(rw, req)
	if c.Value != nonce {
		log.Printf("%s csrf token mismatch, potential attack", remoteAddr)
		http.Error(rw, "Permission Denied", 403)
		return
	}

	if !strings.HasPrefix(redirect, "/") || strings.HasPrefix(redirect, "//") {
		redirect = "/"
	}

	// set cookie, or deny
	if p.Validator(session.Email) && p.ValidateGroup(session.Email) {
		log.Printf("%s authentication complete %s", remoteAddr, session)
		err := p.SaveSession(rw, req, session)
		if err != nil {
			log.Printf("%s %s", remoteAddr, err)
			http.Error(rw, "Internal Error", 500)
			return
		}
		http.Redirect(rw, req, redirect, 302)
	} else {
		log.Printf("%s Permission Denied: %q is unauthorized", remoteAddr, session.Email)
		http.Error(rw, "Permission Denied", 403)
	}
}
