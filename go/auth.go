package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	// signInTimeout is how long the browser flow may stay open. Trae's own
	// client allows 10 minutes.
	signInTimeout = 10 * time.Minute
	// refreshLead is how early the host should renew a JWT.
	refreshLead = 10 * time.Minute
	// day is the fallback lifetime of a token that names no end.
	day = 24 * time.Hour
	// callbackPath is the one path Trae's page is sent back to.
	callbackPath = "/authorize"
)

// startLogin begins a browser sign-in on the selected portal. Trae's page sends
// the browser back to a callback on 127.0.0.1 carrying the account's JWT, so the
// plugin opens one listener for this flow and stores what it receives beside the
// auth material; the host polls until it is there.
func startLogin(req pluginapi.AuthLoginStartRequest, sender httpSender) (pluginapi.AuthLoginStartResponse, error) {
	var out pluginapi.AuthLoginStartResponse
	if req.Host.AuthDir == "" {
		return out, errors.New("CPA auth directory unavailable")
	}
	spec, err := loginSite(req.Provider, req.Metadata, getConfig().Site)
	if err != nil {
		return out, err
	}
	key, err := loadStateKey(req.Host.AuthDir)
	if err != nil {
		return out, err
	}
	flow, err := newNonce()
	if err != nil {
		return out, errors.New("cannot generate login flow")
	}
	dev, err := newDevice(spec)
	if err != nil {
		return out, err
	}
	if err := saveDevice(req.Host.AuthDir, flow, dev); err != nil {
		return out, err
	}
	callback, err := startCallbackFlow(req.Host.AuthDir, flow)
	if err != nil {
		_ = removeDevice(req.Host.AuthDir, flow)
		return out, err
	}
	expires := time.Now().Add(signInTimeout)
	state, err := sealState(key, loginState{Site: spec.ID, Expires: expires.Unix(), Flow: flow})
	if err != nil {
		closeCallbackFlow(req.Host.AuthDir, flow)
		return out, err
	}
	trace, err := newNonce()
	if err != nil {
		closeCallbackFlow(req.Host.AuthDir, flow)
		return out, errors.New("cannot generate login trace")
	}
	q := url.Values{
		"login_version":     {"1"},
		"auth_from":         {"solo"},
		"login_channel":     {"native_ide"},
		"plugin_version":    {spec.PluginVersion},
		"auth_type":         {"local"},
		"client_id":         {spec.ClientID},
		"redirect":          {"0"},
		"login_trace_id":    {uuidString(trace)},
		"auth_callback_url": {callback},
		"machine_id":        {dev.MachineID},
		"device_id":         {dev.DeviceID},
		"x_device_id":       {dev.DeviceID},
		"x_machine_id":      {dev.MachineID},
		"x_device_brand":    {spec.Brand},
		"x_device_type":     {"windows"},
		"x_os_version":      {"Windows 10 Pro"},
		"x_env":             {""},
		"x_app_version":     {spec.IDEVersion},
		"x_app_type":        {"stable"},
		"hide_saas_login":   {"false"},
	}
	return pluginapi.AuthLoginStartResponse{
		Provider:  credentialProvider,
		URL:       spec.Web + "/authorization?" + q.Encode(),
		State:     state,
		ExpiresAt: expires.UTC(),
	}, nil
}

// loginSite resolves the portal: the entry the request names wins, then an
// explicit metadata hint, then the configured fallback, then trae.ai. An
// invalid explicit value is an error rather than a silent fallback.
func loginSite(provider string, metadata map[string]any, configured string) (siteSpec, error) {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "":
		// A legacy panel that knows only the plugin key sends no provider.
	case pluginStoreID:
		// Same fallback path: the store identifier is not a login entry.
	case siteTrae, siteTraeCN:
		if spec, ok := siteFor(strings.ToLower(strings.TrimSpace(provider))); ok {
			return spec, nil
		}
	default:
		return siteSpec{}, errors.New("unknown login entry: use trae or trae-cn")
	}
	site := configured
	if raw, present := metadata["site"]; present {
		value, ok := raw.(string)
		if !ok {
			return siteSpec{}, errors.New("invalid login site: use trae or trae-cn")
		}
		site = strings.ToLower(strings.TrimSpace(value))
	}
	if site == "" {
		site = siteTrae
	}
	spec, ok := siteFor(site)
	if !ok {
		return siteSpec{}, errors.New("invalid login site: use trae or trae-cn")
	}
	return spec, nil
}

// pollLogin asks whether the browser flow finished, and on success reads the
// account and binds its JWT to the device the page was opened with.
func pollLogin(req pluginapi.AuthLoginPollRequest, sender httpSender) (pluginapi.AuthLoginPollResponse, error) {
	pending := pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusPending}
	key, err := loadStateKey(req.Host.AuthDir)
	if err != nil {
		return pending, err
	}
	s, err := openState(key, req.State)
	if err != nil {
		return pending, err
	}
	// Two polls of one flow must not both read the callback and spend the
	// same refresh token. The lock is the flow's own, so another sign-in is
	// not held behind it.
	unlock := lockFlow(hex.EncodeToString(s.Flow))
	defer unlock()
	spec, _ := siteFor(s.Site)
	if time.Now().Unix() >= s.Expires {
		closeCallbackFlow(req.Host.AuthDir, s.Flow)
		return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: spec.Realm + " sign-in expired; sign in again"}, nil
	}
	values, ok, err := readCallback(req.Host.AuthDir, s.Flow)
	if err != nil {
		return pending, err
	}
	if !ok {
		return pending, nil
	}
	dev, err := readDevice(req.Host.AuthDir, s.Flow)
	if err != nil {
		closeCallbackFlow(req.Host.AuthDir, s.Flow)
		return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: "Trae sign-in flow expired; sign in again"}, nil
	}
	if e := firstNonEmpty(values.Get("error_description"), values.Get("error")); e != "" {
		closeCallbackFlow(req.Host.AuthDir, s.Flow)
		return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: spec.Realm + " sign-in was declined: " + truncate(e, 200)}, nil
	}
	c, err := signedIn(sender, spec, values, dev)
	closeCallbackFlow(req.Host.AuthDir, s.Flow)
	if err != nil {
		return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: err.Error()}, nil
	}
	return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusSuccess, Auth: authFor(c)}, nil
}

// signedIn reads Trae's callback: userJwt (the token pair) and userInfo (the
// account), or, from an older page, a refresh token alone, which is exchanged
// for the pair. The token the page gives is bound to no device, so it is
// renewed for this one at once where the site supports it; when Trae won't, it
// does as is.
func signedIn(sender httpSender, spec siteSpec, q url.Values, dev device) (credential, error) {
	var c credential
	jwt := parseObject(q.Get("userJwt"))
	info := parseUserInfo([]byte(q.Get("userInfo")))
	host := firstNonEmpty(q.Get("host"), info.Host)
	authHost := authHostOf(firstNonEmpty(q.Get("authHost"), q.Get("auth_host"), info.authHost()), spec)
	c = credential{
		Type:     credentialProvider,
		Site:     spec.ID,
		Access:   firstNonEmpty(rawString(jwt["Token"]), rawString(jwt["token"])),
		Refresh:  firstNonEmpty(rawString(jwt["RefreshToken"]), rawString(jwt["refreshToken"]), q.Get("refreshToken")),
		Expires:  whenOf(rawJSON(firstAny(jwt["TokenExpireAt"], jwt["tokenExpireAt"]))),
		ClientID: firstNonEmpty(rawString(jwt["ClientID"]), rawString(jwt["clientId"]), q.Get("clientId"), spec.ClientID),
		UID:      info.id(),
		Name:     info.name(),
		Email:    info.email(),
		API:      traeHost(host),
		Region:   info.region(),
		Host:     traeHost(host),
		AuthHost: authHost,
		Account:  accountOf(q),
		DeviceID: dev.DeviceID, MachineID: dev.MachineID,
		DevicePublicKey: dev.DevicePublicKey, DevicePrivateKey: dev.DevicePrivateKey,
	}
	if c.UID == "" {
		c.UID = firstNonEmpty(q.Get("userId"))
	}
	if c.Access == "" && c.Refresh != "" {
		x, err := exchange(sender, spec, firstNonEmpty(authHost, spec.Auth), c.Refresh, c.ClientID)
		if err != nil {
			return c, err
		}
		c.Access, c.Refresh, c.Expires, c.ClientID = x.Access, x.Refresh, x.Expires, firstNonEmpty(x.ClientID, c.ClientID)
		c.UID = firstNonEmpty(c.UID, x.UID)
		c.Name = firstNonEmpty(c.Name, x.Name)
		c.Email = firstNonEmpty(c.Email, x.Email)
		c.API = firstNonEmpty(c.API, x.API)
		c.Region = firstNonEmpty(c.Region, x.Region)
	}
	if c.Access == "" {
		return c, errors.New(spec.WebHost + " sent back no token")
	}
	if c.Refresh == "" {
		return c, errors.New(spec.WebHost + " sent back no refresh token")
	}
	if c.Expires == 0 {
		c.Expires = jwtExp(c.Access)
	}
	if spec.DeviceExchange {
		if x, err := boundExchange(sender, spec, firstNonEmpty(authHost, spec.Auth), c); err == nil {
			c.Access = firstNonEmpty(x.Access, c.Access)
			c.Refresh = firstNonEmpty(x.Refresh, c.Refresh)
			if x.Expires > 0 {
				c.Expires = x.Expires
			}
			c.DevicePublicKey, c.DevicePrivateKey = x.DevicePublicKey, x.DevicePrivateKey
		}
	}
	if c.UID == "" {
		c.UID = firstNonEmpty(c.Name, c.Email)
	}
	if c.UID == "" {
		return c, errors.New(spec.Realm + ": the sign-in named no user")
	}
	return c, nil
}

// parseObject reads a JSON object the page sent as a string, or an object.
func parseObject(raw string) map[string]any {
	out := map[string]any{}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return out
	}
	_ = json.Unmarshal([]byte(raw), &out)
	return out
}

func rawJSON(v any) json.RawMessage {
	if v == nil {
		return nil
	}
	raw, _ := json.Marshal(v)
	return raw
}

// authHostOf is the host a sign-in named for its tokens, when it named one on
// the site's own domain (an enterprise's may differ from the consumer one);
// "" when it named none, and the site's own is used.
func authHostOf(h string, spec siteSpec) string {
	t := traeHost(h)
	if t == "" {
		return ""
	}
	u, err := parseHost(t)
	if err != nil {
		return ""
	}
	n := strings.ToLower(u.Hostname())
	if n == spec.WebHost || strings.HasSuffix(n, "."+spec.WebHost) {
		return t
	}
	return ""
}

// accountOf keeps the account fields of the sign-in that say what kind of
// account it is (tenant, enterprise, login type), plain values only and never
// anything that looks like a secret. Nothing sends them.
func accountOf(q url.Values) map[string]any {
	info := parseObject(q.Get("userInfo"))
	out := map[string]any{}
	for key, value := range info {
		lower := strings.ToLower(key)
		if !matchesAny(lower, "tenant", "enterprise", "saas", "login", "org", "team", "sso", "usertype", "accounttype") {
			continue
		}
		if matchesAny(lower, "token", "secret", "key", "password", "cookie", "session", "jwt") {
			continue
		}
		switch value.(type) {
		case string, float64, bool, nil:
			out[key] = value
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// authFor maps a credential to the auth record CPA persists.
func authFor(c credential) pluginapi.AuthData {
	raw, _ := json.Marshal(c)
	sum := sha256.Sum256([]byte(c.Site + "\x00" + c.UID))
	id := "trae-" + hex.EncodeToString(sum[:12]) + ".json"
	label := c.label()
	return pluginapi.AuthData{
		Provider:         credentialProvider,
		ID:               id,
		FileName:         id,
		Label:            label,
		StorageJSON:      raw,
		Metadata:         map[string]any{"email": label, "type": credentialProvider, "oauth_provider": c.Site},
		NextRefreshAfter: nextRefresh(c),
	}
}

func nextRefresh(c credential) time.Time {
	now := time.Now()
	at := c.expiry()
	if at.IsZero() {
		return now.Add(refreshLead)
	}
	next := at.Add(-refreshLead)
	if next.Before(now.Add(time.Minute)) {
		return now.Add(time.Minute)
	}
	return next
}

// refreshCredential renews the JWT once. Trae's refresh tokens are single-use,
// so a concurrent call for the same old pair waits for the first result
// instead of spending it twice.
func refreshCredential(req pluginapi.AuthRefreshRequest, sender httpSender) (pluginapi.AuthRefreshResponse, error) {
	var out pluginapi.AuthRefreshResponse
	c, err := readCredential(req.StorageJSON)
	if err != nil {
		return out, errors.New("invalid Trae credential")
	}
	spec, _ := siteFor(c.Site)
	if strings.TrimSpace(c.Refresh) == "" {
		return out, signInExpired(spec)
	}
	refreshed, err := renewCredential(req.AuthID, sender, spec, c)
	if err != nil {
		return out, err
	}
	raw, _ := json.Marshal(refreshed)
	metadata := refreshMetadata(req, refreshed)
	metadata["email"] = refreshed.label()
	metadata["type"] = credentialProvider
	metadata["oauth_provider"] = refreshed.Site
	out = pluginapi.AuthRefreshResponse{
		Auth: pluginapi.AuthData{
			Provider:         credentialProvider,
			ID:               req.AuthID,
			Label:            refreshed.label(),
			StorageJSON:      raw,
			Metadata:         metadata,
			NextRefreshAfter: nextRefresh(refreshed),
		},
		NextRefreshAfter: nextRefresh(refreshed),
	}
	return out, nil
}

type refreshFlight struct {
	from   string
	done   chan struct{}
	result credential
	err    error
}

var refreshMu sync.Mutex
var refreshFlights = map[string]*refreshFlight{}

func renewCredential(authID string, sender httpSender, spec siteSpec, c credential) (credential, error) {
	key := authID
	if key == "" {
		key = c.Site + "\x00" + c.UID
	}
	refreshMu.Lock()
	if flight := refreshFlights[key]; flight != nil {
		refreshMu.Unlock()
		<-flight.done
		if flight.from == c.Refresh {
			if flight.err != nil {
				return credential{}, flight.err
			}
			return flight.result, nil
		}
		return renewOnce(sender, spec, c)
	}
	flight := &refreshFlight{from: c.Refresh, done: make(chan struct{})}
	refreshFlights[key] = flight
	refreshMu.Unlock()

	flight.result, flight.err = renewOnce(sender, spec, c)
	refreshMu.Lock()
	delete(refreshFlights, key)
	refreshMu.Unlock()
	close(flight.done)
	if flight.err != nil {
		return credential{}, flight.err
	}
	return flight.result, nil
}

func renewOnce(sender httpSender, spec siteSpec, c credential) (credential, error) {
	next, err := renewal(sender, spec, c)
	if err != nil {
		return credential{}, err
	}
	next.Site = c.Site
	next.Type = credentialProvider
	next.UID = firstNonEmpty(next.UID, c.UID)
	next.Name = firstNonEmpty(next.Name, c.Name)
	next.Email = firstNonEmpty(next.Email, c.Email)
	next.DeviceID = c.DeviceID
	next.MachineID = c.MachineID
	next.API = firstNonEmpty(next.API, c.API)
	next.Region = firstNonEmpty(next.Region, c.Region)
	next.AuthHost = firstNonEmpty(next.AuthHost, c.AuthHost)
	next.Host = firstNonEmpty(next.Host, c.Host)
	next.Account = c.Account
	if next.Expires == 0 {
		next.Expires = time.Now().Add(day).UnixMilli()
	}
	if next.DevicePublicKey == "" {
		next.DevicePublicKey, next.DevicePrivateKey = c.DevicePublicKey, c.DevicePrivateKey
	}
	return next, nil
}

// parseAuth recognizes an existing "type": "trae" auth file and republishes it.
func parseAuth(req pluginapi.AuthParseRequest) pluginapi.AuthParseResponse {
	switch strings.ToLower(strings.TrimSpace(req.Provider)) {
	case "", credentialProvider, siteTraeCN, pluginStoreID:
	default:
		return pluginapi.AuthParseResponse{}
	}
	var persisted struct {
		Type  string `json:"type"`
		Email string `json:"email"`
	}
	if json.Unmarshal(req.RawJSON, &persisted) != nil {
		return pluginapi.AuthParseResponse{}
	}
	if persisted.Type != "" && persisted.Type != credentialProvider {
		return pluginapi.AuthParseResponse{}
	}
	c, err := readCredential(req.RawJSON)
	if err != nil {
		return pluginapi.AuthParseResponse{}
	}
	name := req.FileName
	if name == "" {
		name = req.Path
	}
	label := strings.TrimSpace(persisted.Email)
	if label == "" {
		label = c.label()
	}
	// The auth file also carries the host-managed metadata the management API
	// writes next to the credential (note, prefix, priority, proxy_url, and the
	// rest). CPA rebuilds the runtime record from this map on every reload, so a
	// parser that answers with identity keys alone makes those fields look
	// unpersisted the moment the file is re-read.
	metadata := persistedMetadata(req.RawJSON, c)
	metadata["email"] = label
	metadata["type"] = credentialProvider
	metadata["oauth_provider"] = c.Site
	a := pluginapi.AuthData{
		Provider:    credentialProvider,
		ID:          name,
		FileName:    req.FileName,
		Label:       label,
		StorageJSON: append([]byte(nil), req.RawJSON...),
		Metadata:    metadata,
	}
	a.NextRefreshAfter = nextRefresh(c)
	return pluginapi.AuthParseResponse{Handled: true, Auth: a}
}

// persistedMetadata returns the host-managed fields of an auth file: everything
// the management API writes next to the credential (note, prefix, priority,
// proxy_url, headers, excluded models, model aliases, request retry, ...). CPA
// rebuilds the runtime record from this map on every reload, so a parser that
// answers with identity keys alone makes those fields read back as unpersisted.
//
// The credential's own fields are removed: they are carried by StorageJSON, and
// the host merges metadata over storage when it saves, so echoing them here
// would let a value captured at parse time overwrite a refreshed credential.
func persistedMetadata(raw []byte, own any) map[string]any {
	var metadata map[string]any
	if json.Unmarshal(raw, &metadata) != nil || metadata == nil {
		return make(map[string]any)
	}
	for _, key := range credentialJSONKeys(own) {
		delete(metadata, key)
	}
	return metadata
}

// refreshMetadata carries the host-managed fields through a token refresh. CPA
// saves the record returned here over the persisted file and treats a field the
// refresh omits as deleted, so dropping the note (or prefix, priority, ...)
// would erase it from the credential. The runtime map wins; fields only the
// file still holds fill the gaps.
func refreshMetadata(req pluginapi.AuthRefreshRequest, own any) map[string]any {
	metadata := make(map[string]any, len(req.Metadata))
	for key, value := range req.Metadata {
		metadata[key] = value
	}
	for key, value := range persistedMetadata(req.StorageJSON, own) {
		if _, exists := metadata[key]; !exists {
			metadata[key] = value
		}
	}
	for _, key := range credentialJSONKeys(own) {
		delete(metadata, key)
	}
	return metadata
}

// credentialJSONKeys lists the JSON names a credential struct owns, so
// persistedMetadata can keep them out of the host-managed map.
func credentialJSONKeys(own any) []string {
	t := reflect.TypeOf(own)
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == nil || t.Kind() != reflect.Struct {
		return nil
	}
	keys := make([]string, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		if name == "" || name == "-" {
			continue
		}
		keys = append(keys, name)
	}
	return keys
}

// ---- callback listener -------------------------------------------------------

type callbackFlow struct {
	key      string
	server   *http.Server
	listener net.Listener
	authDir  string
}

var flowsMu sync.Mutex
var activeFlows = map[string]*callbackFlow{}

// pollLocks serializes the polls of one flow.
var pollLocks sync.Map

func lockFlow(key string) func() {
	value, _ := pollLocks.LoadOrStore(key, &sync.Mutex{})
	mu := value.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

func flowFile(authDir, flow, suffix string) string {
	return filepath.Join(authDir, ".trae-oauth-"+flow+suffix)
}

func saveDevice(authDir string, flow []byte, d device) error {
	raw, err := json.Marshal(d)
	if err != nil {
		return errors.New("cannot encode device identity")
	}
	return writeAtomic(flowFile(authDir, hex.EncodeToString(flow), ".device.json"), raw)
}

func readDevice(authDir string, flow []byte) (device, error) {
	var d device
	raw, err := os.ReadFile(flowFile(authDir, hex.EncodeToString(flow), ".device.json"))
	if err != nil {
		return d, errors.New("sign-in device unavailable")
	}
	if json.Unmarshal(raw, &d) != nil || d.DeviceID == "" || d.MachineID == "" {
		return d, errors.New("sign-in device unavailable")
	}
	return d, nil
}

func removeDevice(authDir string, flow []byte) error {
	return os.Remove(flowFile(authDir, hex.EncodeToString(flow), ".device.json"))
}

func readCallback(authDir string, flow []byte) (url.Values, bool, error) {
	raw, err := os.ReadFile(flowFile(authDir, hex.EncodeToString(flow), ".callback.json"))
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, errors.New("cannot read the sign-in callback")
	}
	var values map[string][]string
	if json.Unmarshal(raw, &values) != nil {
		return nil, false, errors.New("invalid sign-in callback")
	}
	return url.Values(values), true, nil
}

func writeCallback(authDir string, flow []byte, q url.Values) error {
	raw, err := json.Marshal(map[string][]string(q))
	if err != nil {
		return errors.New("cannot encode the sign-in callback")
	}
	return writeAtomic(flowFile(authDir, hex.EncodeToString(flow), ".callback.json"), raw)
}

func writeAtomic(path string, raw []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return errors.New("cannot create auth directory")
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".trae-oauth-*")
	if err != nil {
		return errors.New("cannot write sign-in state")
	}
	name := tmp.Name()
	_, writeErr := tmp.Write(raw)
	closeErr := tmp.Close()
	if writeErr != nil || closeErr != nil {
		_ = os.Remove(name)
		return errors.New("cannot write sign-in state")
	}
	if err := os.Chmod(name, 0600); err != nil {
		_ = os.Remove(name)
		return errors.New("cannot write sign-in state")
	}
	if err := os.Rename(name, path); err != nil {
		// Windows refuses to rename over an existing file; a second callback
		// for the same flow replaces the first.
		_ = os.Remove(path)
		if err = os.Rename(name, path); err != nil {
			_ = os.Remove(name)
			return errors.New("cannot write sign-in state")
		}
	}
	return nil
}

// startCallbackFlow opens the one listener this sign-in uses and returns its
// URL. The page sends the browser back to it with the account's JWT.
func startCallbackFlow(authDir string, flow []byte) (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", errors.New("cannot open the Trae sign-in callback listener")
	}
	key := hex.EncodeToString(flow)
	mux := http.NewServeMux()
	mux.HandleFunc(callbackPath, func(w http.ResponseWriter, r *http.Request) {
		// The site's page may call the callback from script as well as open it.
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.Header().Set("Cache-Control", "no-store")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		q := r.URL.Query()
		if q.Get("error") == "" && q.Get("error_description") == "" && q.Get("userJwt") == "" && q.Get("refreshToken") == "" {
			writePage(w, false, "Nothing to sign in with", "Trae sent no token; start the sign-in again.")
			return
		}
		if err := writeCallback(authDir, flow, q); err != nil {
			writePage(w, false, "Sign-in didn't finish", "The plugin could not save the sign-in; start it again.")
			return
		}
		writePage(w, true, "You're signed in", "Trae is signed in. You can close this tab.")
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second}
	cf := &callbackFlow{key: key, server: srv, listener: ln, authDir: authDir}
	flowsMu.Lock()
	if prev := activeFlows[key]; prev != nil {
		_ = prev.server.Close()
	}
	activeFlows[key] = cf
	flowsMu.Unlock()
	go func() { _ = srv.Serve(ln) }()
	go func() {
		time.Sleep(signInTimeout)
		closeCallbackFlow(authDir, flow)
	}()
	return "http://127.0.0.1:" + portOf(ln.Addr()) + callbackPath, nil
}

// closeAllFlows ends every sign-in a plugin unload can no longer serve.
func closeAllFlows() {
	flowsMu.Lock()
	flows := make([]*callbackFlow, 0, len(activeFlows))
	for key, cf := range activeFlows {
		delete(activeFlows, key)
		flows = append(flows, cf)
	}
	flowsMu.Unlock()
	for _, cf := range flows {
		if cf == nil {
			continue
		}
		_ = cf.server.Close()
		_ = cf.listener.Close()
		if cf.authDir != "" && cf.key != "" {
			_ = os.Remove(flowFile(cf.authDir, cf.key, ".callback.json"))
			_ = os.Remove(flowFile(cf.authDir, cf.key, ".device.json"))
		}
	}
}

func portOf(addr net.Addr) string {
	if addr == nil {
		return "0"
	}
	_, port, err := net.SplitHostPort(addr.String())
	if err != nil {
		return "0"
	}
	return port
}

// closeCallbackFlow ends one flow and removes its sidecars. It is safe to call
// more than once.
func closeCallbackFlow(authDir string, flow []byte) {
	key := hex.EncodeToString(flow)
	flowsMu.Lock()
	cf := activeFlows[key]
	delete(activeFlows, key)
	flowsMu.Unlock()
	if cf != nil {
		_ = cf.server.Close()
		_ = cf.listener.Close()
	}
	_ = os.Remove(flowFile(authDir, key, ".callback.json"))
	_ = os.Remove(flowFile(authDir, key, ".device.json"))
}

func writePage(w http.ResponseWriter, ok bool, title, text string) {
	color := "#d1453b"
	if ok {
		color = "#2a9d58"
	}
	body := `<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>` +
		htmlEscape(title) + `</title><style>body{font:15px/1.5 -apple-system,system-ui,sans-serif;margin:0;display:grid;place-items:center;min-height:100vh;background:#f6f6f4;color:#222}@media(prefers-color-scheme:dark){body{background:#1c1c1c;color:#eee}}main{max-width:420px;padding:24px}h1{font-size:20px;margin:0 0 8px}p{margin:0;opacity:.75}.dot{display:inline-block;width:10px;height:10px;border-radius:50%;margin-right:8px;background:` + color + `}</style><main><h1><span class="dot"></span>` +
		htmlEscape(title) + `</h1><p>` + htmlEscape(text) + `</p></main>`
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(body))
}

func htmlEscape(s string) string {
	r := strings.NewReplacer("<", "&lt;", ">", "&gt;", "&", "&amp;", `"`, "&quot;")
	return r.Replace(s)
}
