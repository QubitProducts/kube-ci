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

	"github.com/pkg/errors"
)

func getRemoteAddr(r *http.Request) (s string) {
	s = r.RemoteAddr
	if r.Header.Get("X-Real-IP") != "" {
		s += fmt.Sprintf(" (%q)", r.Header.Get("X-Real-IP"))
	}
	return
}

func nonce() (nonce string, err error) {
	b := make([]byte, 16)
	_, err = rand.Read(b)
	if err != nil {
		return
	}
	nonce = fmt.Sprintf("%x", b)
	return
}

// cookies are stored in a 3 part (value + timestamp + signature) to enforce
// that the values are as originally set.  additionally, the 'value' is
// encrypted so it's opaque to the browser Validate ensures a cookie is
// properly signed
func validate(cookie *http.Cookie, seed string, expiration time.Duration) (value string, t time.Time, ok bool) {
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
func signedValue(seed string, key string, value string, now time.Time) string {
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

// cipher provides methods to encrypt and decrypt cookie values
type cookieCipher struct {
	cipher.Block
}

// NewCipher returns a new aes Cipher for encrypting cookie values
func newCipher(secret []byte) (*cookieCipher, error) {
	c, err := aes.NewCipher(secret)
	if err != nil {
		return nil, err
	}
	return &cookieCipher{Block: c}, err
}

// Encrypt a value for use in a cookie
func (c *cookieCipher) encrypt(value string) (string, error) {
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
func (c *cookieCipher) decrypt(s string) (string, error) {
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

func requestUnparsedResponse(url string) (resp *http.Response, err error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}

	return http.DefaultClient.Do(req)
}

type sessionState struct {
	AccessToken  string
	ExpiresOn    time.Time
	RefreshToken string
	Email        string
	User         string
}

func (s *sessionState) isExpired() bool {
	if !s.ExpiresOn.IsZero() && s.ExpiresOn.Before(time.Now()) {
		return true
	}
	return false
}

func (s *sessionState) String() string {
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

func (s *sessionState) encodeSessionState(c *cookieCipher) (string, error) {
	if c == nil || s.AccessToken == "" {
		return s.userOrEmail(), nil
	}
	return s.encryptedString(c)
}

func (s *sessionState) userOrEmail() string {
	u := s.User
	if s.Email != "" {
		u = s.Email
	}
	return u
}

func (s *sessionState) encryptedString(c *cookieCipher) (string, error) {
	var err error
	if c == nil {
		panic("error. missing cipher")
	}
	a := s.AccessToken
	if a != "" {
		a, err = c.encrypt(a)
		if err != nil {
			return "", err
		}
	}
	r := s.RefreshToken
	if r != "" {
		r, err = c.encrypt(r)
		if err != nil {
			return "", err
		}
	}
	return fmt.Sprintf("%s|%s|%d|%s", s.userOrEmail(), a, s.ExpiresOn.Unix(), r), nil
}

func decodeSessionState(v string, c *cookieCipher) (s *sessionState, err error) {
	chunks := strings.Split(v, "|")
	if len(chunks) == 1 {
		if strings.Contains(chunks[0], "@") {
			u := strings.Split(v, "@")[0]
			return &sessionState{Email: v, User: u}, nil
		}
		return &sessionState{User: v}, nil
	}

	if len(chunks) != 4 {
		err = fmt.Errorf("invalid number of fields (got %d expected 4)", len(chunks))
		return
	}

	s = &sessionState{}
	if c != nil && chunks[1] != "" {
		s.AccessToken, err = c.decrypt(chunks[1])
		if err != nil {
			return nil, err
		}
	}
	if c != nil && chunks[3] != "" {
		s.RefreshToken, err = c.decrypt(chunks[3])
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

type OAuthProxy struct {
	host         string // "github.com"
	validateHost string // "api.github.com"
	org          string
	team         string

	cookieSeed     string
	cookieName     string
	csrfCookieName string
	cookieDomain   string
	cookieSecure   bool
	cookieHTTPOnly bool
	cookieExpire   time.Duration
	cookieRefresh  time.Duration

	validator func(string) bool

	redirectURL *url.URL // the url to receive requests at

	cookieCipher      *cookieCipher
	clientID          string
	clientSecret      string
	loginURL          *url.URL
	redeemURL         *url.URL
	protectedResource *url.URL
	validateURL       *url.URL

	mux *http.ServeMux
}

func (p *OAuthProxy) getRedirect(req *http.Request) (redirect string, err error) {
	err = req.ParseForm()
	if err != nil {
		return
	}

	redirect = req.URL.RequestURI()
	if req.Header.Get("X-Auth-Request-Redirect") != "" {
		redirect = req.Header.Get("X-Auth-Request-Redirect")
	}

	if redirect == "" || !strings.HasPrefix(redirect, "/") || strings.HasPrefix(redirect, "//") {
		redirect = "/"
	}

	return
}

func (p *OAuthProxy) redeem(redirectURL, code string) (s *sessionState, err error) {
	if code == "" {
		err = errors.New("missing code")
		return
	}

	params := url.Values{}
	params.Add("redirect_uri", redirectURL)
	params.Add("client_id", p.clientID)
	params.Add("client_secret", p.clientSecret)
	params.Add("code", code)
	params.Add("grant_type", "authorization_code")
	if p.protectedResource != nil && p.protectedResource.String() != "" {
		params.Add("resource", p.protectedResource.String())
	}

	var req *http.Request
	req, err = http.NewRequest("POST", p.redeemURL.String(), bytes.NewBufferString(params.Encode()))
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
		err = fmt.Errorf("got %d from %q %s", resp.StatusCode, p.redeemURL.String(), body)
		return
	}

	// blindly try json and x-www-form-urlencoded
	var jsonResponse struct {
		AccessToken string `json:"access_token"`
	}
	err = json.Unmarshal(body, &jsonResponse)
	if err == nil {
		s = &sessionState{
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
		s = &sessionState{AccessToken: a}
	} else {
		err = fmt.Errorf("no access token found %s", body)
	}
	return
}

// getLoginURL with typical oauth parameters
func (p *OAuthProxy) getLoginURL(redirectURI, state string) string {
	a := *p.loginURL
	params, _ := url.ParseQuery(a.RawQuery)
	params.Set("redirect_uri", redirectURI)
	params.Set("approval_prompt", `force`)
	params.Add("scope", "read:org")
	params.Set("client_id", p.clientID)
	params.Set("response_type", "code")
	params.Add("state", state)
	a.RawQuery = params.Encode()
	return a.String()
}

// cookieForSession serializes a session state for storage in a cookie
func (p *OAuthProxy) cookieForSession(s *sessionState, c *cookieCipher) (string, error) {
	return s.encodeSessionState(c)
}

// sessionFromCookie deserializes a session from a cookie value
func (p *OAuthProxy) sessionFromCookie(v string, c *cookieCipher) (s *sessionState, err error) {
	return decodeSessionState(v, c)
}

// validateGroup validates that the provided email exists in the configured provider
// email group(s).
func (p *OAuthProxy) validateGroup(email string) bool {
	return true
}

func (p *OAuthProxy) getRedirectURI(host string) string {
	// default to the request Host if not set
	if p.redirectURL.Host != "" {
		return p.redirectURL.String()
	}

	u := *p.redirectURL
	if u.Scheme == "" {
		if p.cookieSecure {
			u.Scheme = "https"
		} else {
			u.Scheme = "http"
		}
	}
	u.Host = p.host
	return u.String()
}

func (p *OAuthProxy) redeemCode(host, code string) (s *sessionState, err error) {
	if code == "" {
		return nil, errors.New("missing code")
	}
	redirectURI := p.getRedirectURI(host)
	s, err = p.redeem(redirectURI, code)
	if err != nil {
		return
	}

	if s.Email == "" {
		s.Email, err = p.GetEmailAddress(s)
	}
	return
}

func (p *OAuthProxy) makeSessionCookie(r *http.Request, value string, expiration time.Duration, now time.Time) *http.Cookie {
	if value != "" {
		value = signedValue(p.cookieSeed, p.cookieName, value, now)
		if len(value) > 4096 {
			// Cookies cannot be larger than 4kb
			log.Printf("WARNING - Cookie Size: %d bytes", len(value))
		}
	}
	return p.makeCookie(r, p.cookieName, value, expiration, now)
}

func (p *OAuthProxy) makeCSRFCookie(req *http.Request, value string, expiration time.Duration, now time.Time) *http.Cookie {
	return p.makeCookie(req, p.csrfCookieName, value, expiration, now)
}

func (p *OAuthProxy) makeCookie(r *http.Request, name string, value string, expiration time.Duration, now time.Time) *http.Cookie {
	domain := r.Host
	if h, _, err := net.SplitHostPort(domain); err == nil {
		domain = h
	}
	if p.cookieDomain != "" {
		if !strings.HasSuffix(domain, p.cookieDomain) {
			log.Printf("Warning: request host is %q but using configured cookie domain of %q", domain, p.cookieDomain)
		}
		domain = p.cookieDomain
	}

	return &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		Domain:   domain,
		HttpOnly: p.cookieHTTPOnly,
		Secure:   p.cookieSecure,
		Expires:  now.Add(expiration),
	}
}

// validateToken returns true if token is valid
func (p *OAuthProxy) validateToken(accessToken string) bool {
	if accessToken == "" || p.validateURL == nil {
		return false
	}
	endpoint := p.validateURL.String()
	params := url.Values{"access_token": {accessToken}}
	endpoint = endpoint + "?" + params.Encode()

	resp, err := requestUnparsedResponse(endpoint)
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
		Host:     p.validateHost,
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
		if p.org == org.Login {
			log.Printf("Found Github Organization: %q", org.Login)
			return true, nil
		}
		presentOrgs = append(presentOrgs, org.Login)
	}

	log.Printf("Missing Organization:%q in %v", p.org, presentOrgs)
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
		Host:     p.validateHost,
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
		if p.org == team.Org.Login {
			hasOrg = true
			ts := strings.Split(p.team, ",")
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
		log.Printf("Missing Team:%q from Org:%q in teams: %v", p.team, p.org, presentTeams)
	} else {
		var allOrgs []string
		for org := range presentOrgs {
			allOrgs = append(allOrgs, org)
		}
		log.Printf("Missing Organization:%q in %#v", p.org, allOrgs)
	}
	return false, nil
}

func (p *OAuthProxy) GetEmailAddress(s *sessionState) (string, error) {
	var emails []struct {
		Email   string `json:"email"`
		Primary bool   `json:"primary"`
	}

	// if we require an Org or Team, check that first
	if p.org != "" {
		if p.team != "" {
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
		Host:   p.validateHost,
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

func (p *OAuthProxy) authenticate(w http.ResponseWriter, r *http.Request) int {
	if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
		return http.StatusAccepted
	}

	var saveSession, clearSession, revalidated bool
	remoteAddr := getRemoteAddr(r)

	session, sessionAge, err := p.LoadCookiedSession(r)
	if err != nil {
		log.Printf("%s %s", remoteAddr, err)
	}

	if session != nil && sessionAge > p.cookieRefresh && p.cookieRefresh != time.Duration(0) {
		log.Printf("%s refreshing %s old session cookie for %s (refresh after %s)", remoteAddr, sessionAge, session, p.cookieRefresh)
		saveSession = true
	}

	if session != nil && session.isExpired() {
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

	if session != nil && session.Email != "" && !p.validator(session.Email) {
		log.Printf("%s Permission Denied: removing session %s", remoteAddr, session)
		session = nil
		saveSession = false
		clearSession = true
	}

	if saveSession && session != nil {
		err := p.saveSession(w, r, session)
		if err != nil {
			log.Printf("%s %s", remoteAddr, err)
			return http.StatusInternalServerError
		}
	}

	if clearSession {
		p.clearSessionCookie(w, r)
	}

	if session == nil {
		return http.StatusForbidden
	}

	// At this point, the user is authenticated. proxy normally
	r.Header["X-Forwarded-User"] = []string{session.User}
	if session.Email != "" {
		r.Header["X-Forwarded-Email"] = []string{session.Email}
	}
	w.Header().Set("X-Auth-Request-User", session.User)
	if session.Email != "" {
		w.Header().Set("X-Auth-Request-Email", session.Email)
	}

	r.Header["X-Forwarded-Access-Token"] = []string{session.AccessToken}

	return http.StatusAccepted
}

func (p *OAuthProxy) clearCSRFCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, p.makeCSRFCookie(r, "", time.Hour*-1, time.Now()))
}

func (p *OAuthProxy) setCSRFCookie(w http.ResponseWriter, r *http.Request, val string) {
	http.SetCookie(w, p.makeCSRFCookie(r, val, p.cookieExpire, time.Now()))
}

func (p *OAuthProxy) clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, p.makeSessionCookie(r, "", time.Hour*-1, time.Now()))
}

func (p *OAuthProxy) setSessionCookie(w http.ResponseWriter, r *http.Request, val string) {
	http.SetCookie(w, p.makeSessionCookie(r, val, p.cookieExpire, time.Now()))
}

func (p *OAuthProxy) LoadCookiedSession(r *http.Request) (*sessionState, time.Duration, error) {
	var age time.Duration
	c, err := r.Cookie(p.cookieName)
	if err != nil {
		// always http.ErrNoCookie
		return nil, age, fmt.Errorf("cookie %q not present", p.cookieName)
	}
	val, timestamp, ok := validate(c, p.cookieSeed, p.cookieExpire)
	if !ok {
		return nil, age, errors.New("Cookie Signature not valid")
	}

	session, err := p.sessionFromCookie(val, p.cookieCipher)
	if err != nil {
		return nil, age, err
	}

	age = time.Now().Truncate(time.Second).Sub(timestamp)
	return session, age, nil
}

func (p *OAuthProxy) saveSession(w http.ResponseWriter, r *http.Request, s *sessionState) error {
	value, err := p.cookieForSession(s, p.cookieCipher)
	if err != nil {
		return err
	}
	p.setSessionCookie(w, r, value)
	return nil
}

func (p *OAuthProxy) oauthStart(w http.ResponseWriter, r *http.Request) {
	nonce, err := nonce()
	if err != nil {
		http.Error(w, "Internal Error", http.StatusInternalServerError)
		return
	}
	p.setCSRFCookie(w, r, nonce)
	redirect, err := p.getRedirect(r)
	if err != nil {
		http.Error(w, "Internal Error", http.StatusInternalServerError)
		return
	}
	redirectURI := p.getRedirectURI(r.Host)
	http.Redirect(w, r, p.getLoginURL(redirectURI, fmt.Sprintf("%v:%v", nonce, redirect)), http.StatusFound)
}

func (p *OAuthProxy) OAuthCallback(rw http.ResponseWriter, req *http.Request) {
	remoteAddr := getRemoteAddr(req)

	// finish the oauth cycle
	err := req.ParseForm()
	if err != nil {
		http.Error(rw, "Internal Error", http.StatusInternalServerError)
		return
	}
	errorString := req.Form.Get("error")
	if errorString != "" {
		http.Error(rw, "Permission Denied", http.StatusForbidden)
		return
	}

	session, err := p.redeemCode(req.Host, req.Form.Get("token"))
	if err != nil {
		log.Printf("%s error redeeming code %s", remoteAddr, err)
		http.Error(rw, "Internal Error", http.StatusInternalServerError)
		return
	}

	s := strings.SplitN(req.Form.Get("state"), ":", 2)
	if len(s) != 2 {
		http.Error(rw, "Internal Error", http.StatusInternalServerError)
		return
	}
	nonce := s[0]
	redirect := s[1]
	c, err := req.Cookie(p.csrfCookieName)
	if err != nil {
		http.Error(rw, "Permission Denied", http.StatusForbidden)
		return
	}
	p.clearCSRFCookie(rw, req)
	if c.Value != nonce {
		log.Printf("%s csrf token mismatch, potential attack", remoteAddr)
		http.Error(rw, "Permission Denied", http.StatusForbidden)
		return
	}

	if !strings.HasPrefix(redirect, "/") || strings.HasPrefix(redirect, "//") {
		redirect = "/"
	}

	// set cookie, or deny
	if p.validator(session.Email) && p.validateGroup(session.Email) {
		log.Printf("%s authentication complete %s", remoteAddr, session)
		err := p.saveSession(rw, req, session)
		if err != nil {
			log.Printf("%s %s", remoteAddr, err)
			http.Error(rw, "Internal Error", http.StatusInternalServerError)
			return
		}
		http.Redirect(rw, req, redirect, http.StatusFound)
	} else {
		log.Printf("%s Permission Denied: %q is unauthorized", remoteAddr, session.Email)
		http.Error(rw, "Permission Denied", http.StatusForbidden)
	}
}

func (p *OAuthProxy) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	status := p.authenticate(w, req)
	if status == http.StatusInternalServerError {
		http.Error(w, http.StatusText(status), status)
	} else if status == http.StatusForbidden {
		p.oauthStart(w, req)
	} else {
		p.mux.ServeHTTP(w, req)
	}
}

func newOAuthProxy() *OAuthProxy {
	secret := []byte("")
	p := &OAuthProxy{
		cookieCipher: newCipher(secret),

		loginURL: &url.URL{
			Scheme: "https",
			Host:   "github.com",
			Path:   "/login/oauth/authorize",
		},
		redeemURL: &url.URL{
			Scheme: "https",
			Host:   "github.com",
			Path:   "/login/oauth/access_token",
		},
		validateURL: &url.URL{
			Scheme: "https",
			Host:   "api.github.com",
			Path:   "/",
		},

		/*
			cookieName:     opts.CookieName,
			csrfCookieName: fmt.Sprintf("%v_%v", opts.CookieName, "csrf"),
			cookieSeed:     opts.CookieSecret,
			cookieDomain:   opts.CookieDomain,
			cookieSecure:   opts.CookieSecure,
			cookieHttpOnly: opts.CookieHttpOnly,
			cookieExpire:   opts.CookieExpire,
			cookieRefresh:  opts.CookieRefresh,
		*/

		//Validator:      validator,
		//OAuthCallbackPath: fmt.Sprintf("%s/callback", opts.ProxyPrefix),
		//redirectURL:        redirectURL,

	}

	return p
}
