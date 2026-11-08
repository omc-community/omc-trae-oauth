package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// expiredError marks a sign-in the vendor refused: the host must ask the user
// to sign in again rather than retry the refresh.
type expiredError struct{ message string }

func (e *expiredError) Error() string { return e.message }

func signInExpired(spec siteSpec) error {
	return &expiredError{message: spec.Realm + " sign-in has expired; sign in again"}
}

// device is the machine an account signs in from, made once at sign-in and
// named to the authorization page, then sent with every request. Trae CN's id
// has the shape its clients' have (16 digits); the international site keeps
// the 19 it was signed in with.
type device struct {
	DeviceID         string `json:"device_id"`
	MachineID        string `json:"machine_id"`
	DevicePublicKey  string `json:"device_public_key,omitempty"`
	DevicePrivateKey string `json:"device_private_key,omitempty"`
}

func newDevice(spec siteSpec) (device, error) {
	n := 19
	if spec.ID == siteTraeCN {
		n = 16
	}
	id := digits(n)
	machine := randomHex(16)
	if id == "" || machine == "" {
		return device{}, errors.New("cannot generate device identity")
	}
	key, err := newDeviceKey()
	if err != nil {
		return device{}, err
	}
	return device{DeviceID: id, MachineID: machine, DevicePublicKey: key.public, DevicePrivateKey: key.private}, nil
}

type deviceKey struct {
	public  string
	private string
}

func newDeviceKey() (deviceKey, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return deviceKey{}, errors.New("cannot generate device key")
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return deviceKey{}, errors.New("cannot encode device key")
	}
	pubDER, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		return deviceKey{}, errors.New("cannot encode device public key")
	}
	return deviceKey{
		public:  string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})),
		private: string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
	}, nil
}

func parseDeviceKey(privatePEM string) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(privatePEM))
	if block == nil {
		return nil, errors.New("invalid device key")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("invalid device key")
	}
	ec, ok := key.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("invalid device key")
	}
	return ec, nil
}

// proofOf is a renewal's DeviceProof: the device key's ECDSA-SHA256 signature
// (DER, base64) of the method, path, client, refresh token, time and nonce,
// one to a line, as the clients sign it.
func proofOf(path, clientID, refresh, privatePEM string, at int64, nonce string) (signature string, err error) {
	key, err := parseDeviceKey(privatePEM)
	if err != nil {
		return "", err
	}
	text := strings.Join([]string{"POST", path, clientID, refresh, strconv.FormatInt(at, 10), nonce}, "\n")
	sum := sha256.Sum256([]byte(text))
	der, err := ecdsa.SignASN1(rand.Reader, key, sum[:])
	if err != nil {
		return "", errors.New("cannot sign the device proof")
	}
	return base64.StdEncoding.EncodeToString(der), nil
}

// tokenResult is a token exchange's answer, in the shape Trae wraps it.
type tokenResult struct {
	Token        string          `json:"Token"`
	TokenLower   string          `json:"token"`
	Refresh      string          `json:"RefreshToken"`
	RefreshLower string          `json:"refreshToken"`
	ExpireAt     json.RawMessage `json:"TokenExpireAt"`
	ExpireLower  json.RawMessage `json:"tokenExpireAt"`
	ClientID     string          `json:"ClientID"`
	ClientLower  string          `json:"clientId"`
	UserID       string          `json:"UserID"`
	UserIDLower  string          `json:"userId"`
	UserInfo     json.RawMessage `json:"UserInfo"`
	UserInfoLow  json.RawMessage `json:"userInfo"`
}

type tokenEnvelope struct {
	Result      *tokenResult    `json:"Result"`
	ResultLower *tokenResult    `json:"result"`
	Code        json.RawMessage `json:"code"`
	Message     string          `json:"message"`
	Msg         string          `json:"msg"`
}

type userInfo struct {
	UserID     string `json:"UserID"`
	UserIDAlt  string `json:"userId"`
	ScreenName string `json:"ScreenName"`
	ScreenAlt  string `json:"screenName"`
	Host       string `json:"Host"`
	Region     string `json:"AIRegion"`
	RegionAlt  string `json:"region"`
	Email      string `json:"Email"`
	EmailAlt   string `json:"email"`
	AuthHost   string `json:"AuthHost"`
	AuthHostLw string `json:"authHost"`
}

func (u userInfo) name() string { return firstNonEmpty(u.ScreenName, u.ScreenAlt) }
func (u userInfo) id() string   { return firstNonEmpty(u.UserID, u.UserIDAlt) }
func (u userInfo) email() string {
	return firstNonEmpty(u.Email, u.EmailAlt)
}
func (u userInfo) region() string { return firstNonEmpty(u.Region, u.RegionAlt) }
func (u userInfo) authHost() string {
	return firstNonEmpty(u.AuthHost, u.AuthHostLw)
}

func parseUserInfo(raw json.RawMessage) userInfo {
	var u userInfo
	if len(raw) == 0 {
		return u
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		_ = json.Unmarshal([]byte(text), &u)
		return u
	}
	_ = json.Unmarshal(raw, &u)
	return u
}

func (r tokenResult) token() string   { return firstNonEmpty(r.Token, r.TokenLower) }
func (r tokenResult) refresh() string { return firstNonEmpty(r.Refresh, r.RefreshLower) }
func (r tokenResult) client() string  { return firstNonEmpty(r.ClientID, r.ClientLower) }

// exchange trades a refresh token for a new Cloud-IDE-JWT at the cloudide
// endpoint, as the IDE does. The answer's Result holds the new pair and the
// account. Trae issues a new refresh token each time and spends the old one.
func exchange(sender httpSender, spec siteSpec, host, refresh, clientID string) (credential, error) {
	var out credential
	clientID = firstNonEmpty(clientID, spec.ClientID)
	body, _ := json.Marshal(map[string]string{
		"ClientID":     clientID,
		"RefreshToken": refresh,
		"ClientSecret": "-",
		"UserID":       "",
	})
	resp, err := sender.Do(pluginapi.HTTPRequest{
		Method:  "POST",
		URL:     strings.TrimRight(host, "/") + "/cloudide/api/v3/trae/oauth/ExchangeToken",
		Headers: http.Header{"Content-Type": {"application/json"}},
		Body:    body,
	})
	if err != nil {
		return out, fmt.Errorf("%s: renewing the sign-in failed", spec.Realm)
	}
	r, err := readTokenResult(resp)
	if err != nil {
		return out, renewFailure(spec, resp, err)
	}
	info := parseUserInfo(firstRaw(r.UserInfo, r.UserInfoLow))
	out = credential{
		Site:     spec.ID,
		Access:   r.token(),
		Refresh:  r.refresh(),
		Expires:  whenOf(firstRaw(r.ExpireAt, r.ExpireLower)),
		ClientID: firstNonEmpty(r.client(), clientID),
		UID:      info.id(),
		Name:     info.name(),
		Email:    info.email(),
		API:      info.Host,
		Region:   info.region(),
	}
	if out.Expires == 0 {
		out.Expires = jwtExp(out.Access)
	}
	if out.Refresh == "" {
		out.Refresh = refresh
	}
	return out, nil
}

func readTokenResult(resp pluginapi.HTTPResponse) (tokenResult, error) {
	var env tokenEnvelope
	if json.Unmarshal(resp.Body, &env) == nil {
		r := env.Result
		if r == nil {
			r = env.ResultLower
		}
		if r != nil && r.token() != "" {
			return *r, nil
		}
	}
	// Some answers put the pair at the top level.
	var direct tokenResult
	if json.Unmarshal(resp.Body, &direct) == nil && direct.token() != "" {
		return direct, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return tokenResult{}, &upstreamError{Status: resp.StatusCode, Body: resp.Body}
	}
	return tokenResult{}, &upstreamError{Status: http.StatusBadGateway, Body: resp.Body}
}

// renewFailure turns a token answer into an expired sign-in or a readable
// error, the way Trae's clients read one.
func renewFailure(spec siteSpec, resp pluginapi.HTTPResponse, err error) error {
	e, ok := err.(*upstreamError)
	if !ok {
		return fmt.Errorf("%s: renewing the sign-in failed", spec.Realm)
	}
	code, message := errorOf(e.Body)
	msg := firstNonEmpty(message, strings.TrimSpace(string(e.Body)))
	if resp.StatusCode == 400 || resp.StatusCode == 401 || resp.StatusCode == 403 ||
		code == "10101" || matchesAny(msg, "refresh", "expired", "invalid", "not matched", "revoked", "unauthori") {
		return &expiredError{message: spec.Realm + "'s sign-in has expired (" + truncate(msg, 200) + "); sign in again"}
	}
	return fmt.Errorf("%s: renewing the sign-in: HTTP %d %s", spec.Realm, e.Status, truncate(msg, 200))
}

type upstreamError struct {
	Status int
	Body   []byte
}

func (e *upstreamError) Error() string { return fmt.Sprintf("upstream error HTTP %d", e.Status) }

// deviceInfo is the device a renewal names, its public key with it.
func deviceInfo(d device, spec siteSpec, platform, version string) map[string]any {
	return map[string]any{
		"DeviceID":        d.DeviceID,
		"MachineID":       d.MachineID,
		"PlatformCode":    platform,
		"DeviceType":      "PC",
		"DeviceName":      "",
		"DeviceModel":     "",
		"ClientVersion":   version,
		"DevicePublicKey": d.DevicePublicKey,
		"DeviceBrand":     spec.Brand,
		"DeviceCPU":       "AMD",
		"OSInfo":          "Windows",
		"OSVersion":       "10.0.22631",
	}
}

// boundExchange renews the JWT as Trae CN's clients do, so that it is bound to
// the account's device. An account with no device key is given one first. It
// tries each client the site names and returns the last answer when none takes
// it.
func boundExchange(sender httpSender, spec siteSpec, host string, c credential) (credential, error) {
	if c.DevicePrivateKey == "" || c.DevicePublicKey == "" {
		key, err := newDeviceKey()
		if err != nil {
			return credential{}, err
		}
		c.DevicePublicKey, c.DevicePrivateKey = key.public, key.private
	}
	clients := []struct {
		clientID string
		platform string
		version  string
	}{
		{clientID: "en1oxy7wnw8j9n", platform: "SOLO_PC", version: "0.1.64"},
		{clientID: "ono9krqynydwx5", platform: "IDE_PC", version: "3.3.99"},
	}
	path := "/trae/api/v3/oauth/ExchangeToken"
	var last error
	for _, as := range clients {
		nonce := randomHex(16)
		at := time.Now().Unix()
		proof, err := proofOf(path, as.clientID, c.Refresh, c.DevicePrivateKey, at, nonce)
		if err != nil {
			last = err
			continue
		}
		body, _ := json.Marshal(map[string]any{
			"ClientID":     as.clientID,
			"ClientSecret": "",
			"RefreshToken": c.Refresh,
			"DeviceInfo":   deviceInfo(device{DeviceID: c.DeviceID, MachineID: c.MachineID, DevicePublicKey: c.DevicePublicKey}, spec, as.platform, as.version),
			"DeviceProof": map[string]any{
				"Signature": proof,
				"Timestamp": at,
				"Nonce":     nonce,
			},
			"IDEVersion": as.version,
		})
		headers := http.Header{"Content-Type": {"application/json"}}
		if c.Access != "" {
			headers.Set("Authorization", "Cloud-IDE-JWT "+c.Access)
		}
		resp, err := sender.Do(pluginapi.HTTPRequest{
			Method:  "POST",
			URL:     strings.TrimRight(host, "/") + path,
			Headers: headers,
			Body:    body,
		})
		if err != nil {
			last = fmt.Errorf("%s: renewing the sign-in for its device failed", spec.Realm)
			continue
		}
		r, err := readTokenResult(resp)
		if err != nil {
			if e, ok := err.(*upstreamError); ok {
				_, msg := errorOf(e.Body)
				last = fmt.Errorf("%s: renewing the sign-in for its device: HTTP %d %s", spec.Realm, e.Status, truncate(msg, 200))
			} else {
				last = err
			}
			continue
		}
		next := c
		next.Access = r.token()
		if r.refresh() != "" {
			next.Refresh = r.refresh()
		}
		next.Expires = whenOf(firstRaw(r.ExpireAt, r.ExpireLower))
		if next.Expires == 0 {
			next.Expires = jwtExp(next.Access)
		}
		return next, nil
	}
	if last == nil {
		last = fmt.Errorf("%s: renewing the sign-in for its device failed", spec.Realm)
	}
	return credential{}, last
}

// renewal renews an account's JWT: bound to its device where the site can,
// else, or when Trae turns that away, at the cloudide endpoint.
func renewal(sender httpSender, spec siteSpec, c credential) (credential, error) {
	host := c.tokenHost(spec)
	if spec.DeviceExchange {
		if next, err := boundExchange(sender, spec, host, c); err == nil {
			return next, nil
		}
	}
	next, err := exchange(sender, spec, host, c.Refresh, c.ClientID)
	if err != nil {
		return credential{}, err
	}
	if next.DevicePrivateKey == "" {
		next.DevicePublicKey, next.DevicePrivateKey = c.DevicePublicKey, c.DevicePrivateKey
	}
	return next, nil
}

// jwtExp is a JWT's exp in milliseconds, 0 when it has none.
func jwtExp(token string) int64 {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return 0
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return 0
	}
	var payload struct {
		Exp json.RawMessage `json:"exp"`
	}
	if json.Unmarshal(raw, &payload) != nil {
		return 0
	}
	return whenOf(payload.Exp)
}

// whenOf reads one of Trae's expiry times: seconds, milliseconds or ISO.
func whenOf(raw json.RawMessage) int64 {
	if len(raw) == 0 || string(raw) == "null" {
		return 0
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		text = strings.TrimSpace(text)
		if text == "" {
			return 0
		}
		if n, err := strconv.ParseInt(text, 10, 64); err == nil {
			return epochMilli(n)
		}
		if at, err := time.Parse(time.RFC3339, text); err == nil {
			return at.UnixMilli()
		}
		for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02T15:04:05"} {
			if at, err := time.Parse(layout, text); err == nil {
				return at.UnixMilli()
			}
		}
		return 0
	}
	var n json.Number
	if json.Unmarshal(raw, &n) == nil {
		if v, err := n.Int64(); err == nil {
			return epochMilli(v)
		}
		if v, err := n.Float64(); err == nil {
			return epochMilli(int64(v))
		}
	}
	return 0
}

func epochMilli(n int64) int64 {
	if n <= 0 {
		return 0
	}
	if n < 1e11 {
		return n * 1000
	}
	return n
}

// errorOf reads what Trae said went wrong, from its several shapes: a business
// {code, message}, {error: {code, message}}, or ByteDance's
// {ResponseMetadata: {Error: {Code, Message}}}.
func errorOf(body []byte) (code, message string) {
	var v map[string]any
	if json.Unmarshal(body, &v) != nil {
		return "", strings.TrimSpace(string(body))
	}
	if meta, ok := v["ResponseMetadata"].(map[string]any); ok {
		if e, ok := meta["Error"].(map[string]any); ok {
			return rawString(e["Code"]), rawString(e["Message"])
		}
	}
	if e, ok := v["error"].(map[string]any); ok {
		return rawString(firstAny(e["code"], e["Code"], v["code"])), rawString(firstAny(e["message"], e["msg"], e["Message"]))
	}
	if e, ok := v["error"].(string); ok && e != "" {
		return rawString(v["code"]), e
	}
	return rawString(v["code"]), rawString(firstAny(v["message"], v["msg"]))
}

func rawString(v any) string {
	switch x := v.(type) {
	case string:
		return strings.TrimSpace(x)
	case json.Number:
		return x.String()
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case nil:
		return ""
	default:
		return strings.TrimSpace(fmt.Sprint(x))
	}
}

func firstAny(values ...any) any {
	for _, v := range values {
		if v != nil {
			if s, ok := v.(string); ok && strings.TrimSpace(s) == "" {
				continue
			}
			return v
		}
	}
	return nil
}

func firstRaw(values ...json.RawMessage) json.RawMessage {
	for _, v := range values {
		if len(v) > 0 && string(v) != "null" {
			return v
		}
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

func matchesAny(text string, words ...string) bool {
	lower := strings.ToLower(text)
	for _, w := range words {
		if strings.Contains(lower, w) {
			return true
		}
	}
	return false
}

func truncate(text string, n int) string {
	text = strings.TrimSpace(text)
	if len(text) <= n {
		return text
	}
	return text[:n]
}
