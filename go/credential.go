package main

import (
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Trae and Trae CN are separate deployments with separate accounts: different
// authorization pages, token hosts and model gateways. They share one
// credential shape ("type": "trae") and one executor; the site field records
// which portal signed the account in.
const (
	siteTrae   = "trae"
	siteTraeCN = "trae-cn"

	// credentialProvider is the provider key every credential this plugin
	// writes carries, and the identifier CPA routes executions and quota by.
	credentialProvider = "trae"

	// defaultClientVersion is the TRAE SOLO build the model gateway is told it
	// serves. Trae offers a model only to clients new enough for it, so this is
	// configuration and a vendor build bump does not need a new release.
	defaultClientVersion = "0.1.69"
	defaultClientCode    = "20260917"
)

// siteSpec is one deployment of Trae: where each part is served and which
// client the requests claim to be.
type siteSpec struct {
	ID string
	// Name is the user-facing portal name.
	Name string
	// Realm is the name errors and quota groups carry.
	Realm string
	// WebHost is the bare host the sign-in copy names.
	WebHost string
	// Web is the authorization page origin.
	Web string
	// Auth serves tokens, the account and credits.
	Auth string
	// API serves model requests (SG for the international deployment).
	API string
	// US is the international deployment's US model gateway.
	US string
	// Pay serves the international deployment's usage page.
	Pay string
	// USPay serves the US deployment's usage page.
	USPay string
	// ClientID is the IDE's OAuth client.
	ClientID string
	// AppID is the x-app-id header the model gateway expects.
	AppID string
	// IDEVersion is the build named to the authorization page.
	IDEVersion string
	// IDEVersionCode is its version code.
	IDEVersionCode string
	// PluginVersion is the IDE plugin version named to the authorization page.
	PluginVersion string
	// Brand is the device brand the IDE's headers name.
	Brand string
	// DeviceExchange reports whether the site's clients renew a JWT bound to
	// the account's device (Trae CN does).
	DeviceExchange bool
	// Lapsed lists the business codes that mean the sign-in is over.
	Lapsed []string
	// Quota lists the business codes that mean the account is out of credits.
	Quota []string
	// Fallback lists the models the site is known to serve when its catalogue
	// cannot be read.
	Fallback []traeModelSeed
	// UsageKind selects the usage page shape: "credits" or "dollar".
	UsageKind string
}

var sites = []siteSpec{
	{
		ID:             siteTrae,
		Name:           "Trae",
		Realm:          "Trae",
		WebHost:        "trae.ai",
		Web:            "https://www.trae.ai",
		Auth:           "https://growsg-normal.trae.ai",
		API:            "https://coresg-normal.trae.ai",
		US:             "https://coreva-normal.trae.ai",
		Pay:            "https://api-sg-central.trae.ai",
		USPay:          "https://api-us-east.trae.ai",
		ClientID:       "ono9krqynydwx5",
		AppID:          "6eefa01c-1036-4c7e-9ca5-d891f63bfcd8",
		IDEVersion:     "3.5.51",
		IDEVersionCode: "20260401",
		PluginVersion:  "2.3.62834",
		Brand:          "ASUS TUF Gaming A15 FA507RM_FA507RM",
		Lapsed:         []string{"1001", "20101", "401"},
		Quota:          []string{"4008", "1005", "4011"},
		Fallback:       traeFallbackCatalogue,
		UsageKind:      "dollar",
	},
	{
		ID:             siteTraeCN,
		Name:           "Trae CN",
		Realm:          "Trae CN",
		WebHost:        "trae.cn",
		Web:            "https://www.trae.cn",
		Auth:           "https://api.trae.cn",
		API:            "https://trae-api-cn.mchost.guru",
		ClientID:       "ono9krqynydwx5",
		AppID:          "6eefa01c-1036-4c7e-9ca5-d891f63bfcd8",
		IDEVersion:     "3.3.65",
		IDEVersionCode: "20260401",
		PluginVersion:  "2.3.24254",
		Brand:          "ASUS TUF Gaming A15 FA507RM_FA507RM",
		DeviceExchange: true,
		Lapsed:         []string{"1001", "401"},
		Quota:          []string{"4008", "1005"},
		Fallback:       traeCNFallbackCatalogue,
		UsageKind:      "credits",
	},
}

func siteFor(id string) (siteSpec, bool) {
	for _, s := range sites {
		if s.ID == id {
			return s, true
		}
	}
	return siteSpec{}, false
}

// credential is the persisted auth storage. Tokens are opaque strings; Expires
// is the JWT's end in milliseconds since the epoch (0 when Trae named no end).
// The device is the machine the account signed in from, named to the
// authorization page and sent with every request: Trae drops requests that
// arrive on a new device each time.
type credential struct {
	Type string `json:"type,omitempty"`
	Site string `json:"site"`
	// Access is the Cloud-IDE-JWT that signs model calls.
	Access string `json:"access"`
	// Refresh trades for a new JWT; Trae spends it once.
	Refresh string `json:"refresh,omitempty"`
	Expires int64  `json:"expires,omitempty"`
	UID     string `json:"uid"`
	Name    string `json:"name,omitempty"`
	Email   string `json:"email,omitempty"`
	// ClientID is the client the sign-in was issued to.
	ClientID  string `json:"client_id,omitempty"`
	DeviceID  string `json:"device_id"`
	MachineID string `json:"machine_id"`
	// DevicePublicKey and DevicePrivateKey bind a renewal to the device
	// (PEM, P-256). An account signed in before this plugin kept one is given
	// a pair at its next renewal.
	DevicePublicKey  string `json:"device_public_key,omitempty"`
	DevicePrivateKey string `json:"device_private_key,omitempty"`
	// API is the model host the sign-in named, when it named one.
	API string `json:"api,omitempty"`
	// Region selects the international deployment's US gateway.
	Region string `json:"region,omitempty"`
	// AuthHost is the token host the sign-in named, when it named one.
	AuthHost string `json:"auth_host,omitempty"`
	// Host is the host the sign-in named, kept as given.
	Host string `json:"host,omitempty"`
	// Account keeps the sign-in's tenant and login-type fields, which say what
	// kind of account it is. Nothing sends them.
	Account map[string]any `json:"account,omitempty"`
}

func readCredential(b []byte) (credential, error) {
	var c credential
	if json.Unmarshal(b, &c) != nil {
		return c, errors.New("invalid Trae credential")
	}
	spec, ok := siteFor(c.Site)
	if !ok || spec.ClientID == "" {
		return c, errors.New("invalid Trae credential")
	}
	if strings.TrimSpace(c.DeviceID) == "" || strings.TrimSpace(c.MachineID) == "" {
		return c, errors.New("invalid Trae credential")
	}
	if strings.TrimSpace(c.Access) == "" || strings.TrimSpace(c.UID) == "" {
		return c, errors.New("invalid Trae credential")
	}
	return c, nil
}

// expiry is when the JWT runs out, zero when unknown.
func (c credential) expiry() time.Time {
	if c.Expires <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(c.Expires).UTC()
}

// label is the account name a card shows: the screen name first, then the uid.
func (c credential) label() string {
	if n := strings.TrimSpace(c.Name); n != "" {
		return n
	}
	if e := strings.TrimSpace(c.Email); e != "" {
		return e
	}
	return strings.TrimSpace(c.UID)
}

// modelHost is the model gateway this account's requests go to: the host the
// sign-in named when it is one of Trae's, else the site's own.
func (c credential) modelHost(spec siteSpec) string {
	if h := traeHost(c.API); h != "" {
		if spec.ID == siteTraeCN {
			if isTraeCNModelHost(h) {
				return h
			}
			return spec.API
		}
		if isTraeGlobalModelHost(h) {
			return h
		}
		if isTraeGlobalUSHost(h) {
			return spec.US
		}
	}
	if spec.ID == siteTrae {
		if isTraeGlobalUSHost(c.API) || isUSRegion(c.Region) {
			return spec.US
		}
	}
	return spec.API
}

// tokenHost is the host a renewal goes to: the one the sign-in named on the
// site's own domain, else the site's own.
func (c credential) tokenHost(spec siteSpec) string {
	if h := traeHost(c.AuthHost); h != "" {
		if u, err := parseHost(h); err == nil {
			n := strings.ToLower(u.Hostname())
			if n == spec.WebHost || strings.HasSuffix(n, "."+spec.WebHost) {
				return h
			}
		}
	}
	return spec.Auth
}
