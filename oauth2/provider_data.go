package oauth2

import (
	"net/url"
)

type ProviderData struct {
	ProviderName      string
	ClientID          string
	ClientSecret      string
	LoginURL          *url.URL
	RedeemURL         *url.URL
	ProfileURL        *url.URL
	ProtectedResource *url.URL
	ValidateURL       *url.URL
	Scope             string
	ApprovalPrompt    string
	JWTKeysURL        *url.URL
}

func (p *ProviderData) Data() *ProviderData { return p }
