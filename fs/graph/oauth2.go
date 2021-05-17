package graph

import (
	"encoding/json"
	"errors"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	authCodeURL     = "https://login.microsoftonline.com/common/oauth2/v2.0/authorize"
	authTokenURL    = "https://login.microsoftonline.com/common/oauth2/v2.0/token"
	authRedirectURL = "https://login.live.com/oauth20_desktop.srf"
	authClientID    = "3470c3fa-bc10-45ab-a0a9-2d30836485d1"
	authFile        = "auth_tokens.json"
)

// Auth represents a set of oauth2 authentication tokens
type Auth struct {
	Account      string `json:"account"`
	ExpiresIn    int64  `json:"expires_in"` // only used for parsing
	ExpiresAt    int64  `json:"expires_at"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	path         string // auth tokens remember their path for use by Refresh()
}

// AuthError is an authentication error from the Microsoft API. Generally we don't see
// these unless something goes catastrophically wrong with Microsoft's authentication
// services.
type AuthError struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
	ErrorCodes       []int  `json:"error_codes"`
	ErrorURI         string `json:"error_uri"`
	Timestamp        string `json:"timestamp"` // json.Unmarshal doesn't like this timestamp format
	TraceID          string `json:"trace_id"`
	CorrelationID    string `json:"correlation_id"`
}

// ToFile writes auth tokens to a file
func (a Auth) ToFile(file string) error {
	a.path = file
	byteData, _ := json.Marshal(a)
	return ioutil.WriteFile(file, byteData, 0600)
}

// FromFile populates an auth struct from a file
func (a *Auth) FromFile(file string) error {
	contents, err := ioutil.ReadFile(file)
	if err != nil {
		return err
	}
	a.path = file
	return json.Unmarshal(contents, a)
}

// Refresh auth tokens if expired.
func (a *Auth) Refresh() {
	if a.ExpiresAt <= time.Now().Unix() {
		oldTime := a.ExpiresAt
		postData := strings.NewReader("client_id=" + authClientID +
			"&redirect_uri=" + authRedirectURL +
			"&refresh_token=" + a.RefreshToken +
			"&grant_type=refresh_token")
		resp, err := http.Post(authTokenURL,
			"application/x-www-form-urlencoded",
			postData)

		var reauth bool
		if err != nil {
			if IsOffline(err) || resp == nil {
				log.WithField("err", err).Trace(
					"Network unreachable during token renewal, ignoring.")
				return
			}
			log.WithField("err", err).Error(
				"Could not POST to renew tokens, forcing reauth.")
			reauth = true
		} else {
			// put here so as to avoid spamming the log when offline
			log.Info("Auth tokens expired, attempting renewal.")
		}
		defer resp.Body.Close()

		body, _ := ioutil.ReadAll(resp.Body)
		json.Unmarshal(body, &a)
		if a.ExpiresAt == oldTime {
			a.ExpiresAt = time.Now().Unix() + a.ExpiresIn
		}

		if reauth || a.AccessToken == "" || a.RefreshToken == "" {
			log.WithFields(log.Fields{
				"response":  string(body),
				"http_code": resp.StatusCode,
			}).Error("Failed to renew access tokens. Attempting to reauthenticate.")
			a = newAuth(a.path)
		} else {
			a.ToFile(a.path)
		}
	}
}

// Get the appropriate authentication URL for the Graph OAuth2 challenge.
func getAuthURL() string {
	return authCodeURL +
		"?client_id=" + authClientID +
		"&scope=" + url.PathEscape("user.read files.readwrite.all offline_access") +
		"&response_type=code" +
		"&redirect_uri=" + authRedirectURL
}

// parseAuthCode is used to parse the auth code out of the redirect the server gives us
// after successful authentication
func parseAuthCode(url string) (string, error) {
	rexp := regexp.MustCompile("code=([a-zA-Z0-9-_.])+")
	code := rexp.FindString(url)
	if len(code) == 0 {
		return "", errors.New("invalid auth code")
	}
	return code[5:], nil
}

// Exchange an auth code for a set of access tokens
func getAuthTokens(authCode string) *Auth {
	postData := strings.NewReader("client_id=" + authClientID +
		"&redirect_uri=" + authRedirectURL +
		"&code=" + authCode +
		"&grant_type=authorization_code")
	resp, err := http.Post(authTokenURL,
		"application/x-www-form-urlencoded",
		postData)
	if err != nil {
		log.WithField("error", err).Fatalf("Could not POST to obtain auth tokens.")
	}
	defer resp.Body.Close()

	body, _ := ioutil.ReadAll(resp.Body)
	var auth Auth
	json.Unmarshal(body, &auth)
	if auth.ExpiresAt == 0 {
		auth.ExpiresAt = time.Now().Unix() + auth.ExpiresIn
	}
	if auth.AccessToken == "" || auth.RefreshToken == "" {
		var authErr AuthError
		var fields log.Fields
		if err := json.Unmarshal(body, &authErr); err == nil {
			// we got a parseable error message out of microsoft's servers
			fields = log.Fields{
				"http_code":         resp.StatusCode,
				"error":             authErr.Error,
				"error_description": authErr.ErrorDescription,
				"help_url":          authErr.ErrorURI,
			}
		} else {
			// things are extra broken and this is an error type we haven't seen before
			fields = log.Fields{
				"http_code":          resp.StatusCode,
				"response":           string(body),
				"response_parse_err": err,
			}
		}
		log.WithFields(fields).Fatalf(
			"Failed to retrieve access tokens. Authentication cannot continue.")
	}
	return &auth
}

// newAuth performs initial authentication flow and saves tokens to disk
func newAuth(path string) *Auth {
	old := Auth{}
	old.FromFile(path)
	auth := getAuthTokens(getAuthCode(old.Account))

	if user, err := GetUser(auth); err == nil {
		auth.Account = user.UserPrincipalName
	}
	auth.ToFile(path)
	return auth
}

// Authenticate performs first-time authentication to Graph
func Authenticate(path string) *Auth {
	auth := &Auth{}
	_, err := os.Stat(path)
	if os.IsNotExist(err) {
		// no tokens found, gotta start oauth flow from beginning
		auth = newAuth(path)
	} else {
		// we already have tokens, no need to force a new auth flow
		auth.FromFile(path)
		auth.Refresh()
	}
	return auth
}
