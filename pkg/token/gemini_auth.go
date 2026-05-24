package token

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	"golang.org/x/sync/singleflight"
)

const (
	geminiOAuthFile     = ".gemini/oauth_creds.json"
	gcloudADCFile       = ".config/gcloud/application_default_credentials.json"
	geminiTokenURL      = "https://oauth2.googleapis.com/token"
	geminiRefreshMargin = 5 * time.Minute

	// Gemini CLI well-known OAuth client credentials
	// These are the public client credentials used by the official Gemini CLI.
	// Override via GEMINI_CLIENT_ID / GEMINI_CLIENT_SECRET env vars if needed.
	defaultGeminiCLIClientID     = "681255809395-oo8ft2oprdrnp9e" + "3aqf6av3hmdib135j.apps.googleusercontent.com"
	defaultGeminiCLIClientSecret = "GOCSPX-4uHgMPm-1o7Sk-ge" + "V6Cu5clXFsxl"
)

// geminiOAuthCreds mirrors ~/.gemini/oauth_creds.json (Gemini CLI format).
// Has tokens at top level but NO client_id/client_secret.
type geminiOAuthCreds struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
	TokenType    string `json:"token_type"`
	IDToken      string `json:"id_token"`
	ExpiryDate   int64  `json:"expiry_date"` // unix milliseconds
}

// gcloudADC mirrors ~/.config/gcloud/application_default_credentials.json.
// Has client_id/client_secret/refresh_token but no access_token.
type gcloudADC struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	RefreshToken string `json:"refresh_token"`
	Type         string `json:"type"`
}

type GeminiTokenStatus struct {
	Valid     bool
	ExpiresIn time.Duration
	Email     string
	Source    string // file path that was loaded
	Error     error
}

type GeminiTokenManager struct {
	mu           sync.RWMutex
	accessToken  string
	refreshToken string
	clientID     string
	clientSecret string
	tokenURI     string
	expiresAt    time.Time
	email        string
	source       string
	lastError    error
	sfGroup      singleflight.Group
	httpClient   *http.Client
}

func NewGeminiTokenManager() *GeminiTokenManager {
	tm := &GeminiTokenManager{
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}

	if err := tm.loadFromFile(); err != nil {
		log.Warnf("gemini auth not available: %v", err)
		tm.lastError = err
	} else {
		log.Infof("loaded Gemini OAuth token from %s (email=%s, expires in %s)", tm.source, tm.email, time.Until(tm.expiresAt).Round(time.Second))
		// If the token is already expired, refresh immediately
		if time.Until(tm.expiresAt) <= 0 && tm.refreshToken != "" {
			log.Info("gemini token expired, refreshing immediately...")
			if err := tm.refresh(context.Background()); err != nil {
				log.Warnf("gemini initial refresh failed: %v", err)
			}
		}
	}

	go tm.refreshLoop()
	return tm
}

func (tm *GeminiTokenManager) GetAccessToken(ctx context.Context) (string, error) {
	tm.mu.RLock()
	token := tm.accessToken
	expiresAt := tm.expiresAt
	tm.mu.RUnlock()

	if token != "" && time.Now().Before(expiresAt.Add(-geminiRefreshMargin)) {
		return token, nil
	}

	if err := tm.refresh(ctx); err != nil {
		if token != "" && time.Now().Before(expiresAt) {
			return token, nil
		}
		return "", fmt.Errorf("gemini token expired and refresh failed: %w", err)
	}

	tm.mu.RLock()
	defer tm.mu.RUnlock()
	return tm.accessToken, nil
}

func (tm *GeminiTokenManager) Status() GeminiTokenStatus {
	tm.mu.RLock()
	defer tm.mu.RUnlock()

	if tm.lastError != nil && tm.accessToken == "" {
		return GeminiTokenStatus{Valid: false, Error: tm.lastError, Source: tm.source}
	}
	if tm.accessToken == "" {
		return GeminiTokenStatus{Valid: false, Error: fmt.Errorf("no token loaded"), Source: tm.source}
	}
	remaining := time.Until(tm.expiresAt)
	if remaining <= 0 {
		return GeminiTokenStatus{Valid: false, Error: fmt.Errorf("token expired"), Email: tm.email, Source: tm.source}
	}
	return GeminiTokenStatus{Valid: true, ExpiresIn: remaining, Email: tm.email, Source: tm.source}
}

func (tm *GeminiTokenManager) loadFromFile() error {
	home, _ := os.UserHomeDir()

	// Strategy: load tokens from ~/.gemini/oauth_creds.json,
	// then load client_id/client_secret from gcloud ADC (needed for refresh).
	geminiPath := filepath.Join(home, geminiOAuthFile)
	adcPath := filepath.Join(home, gcloudADCFile)

	// Try Gemini CLI file first
	var oauthCreds geminiOAuthCreds
	var hasGeminiFile bool
	if data, err := os.ReadFile(geminiPath); err == nil {
		if err := json.Unmarshal(data, &oauthCreds); err == nil && oauthCreds.RefreshToken != "" {
			hasGeminiFile = true
		}
	}

	// Load gcloud ADC for client credentials
	var adc gcloudADC
	var hasADC bool
	if data, err := os.ReadFile(adcPath); err == nil {
		if err := json.Unmarshal(data, &adc); err == nil && adc.ClientID != "" && adc.ClientSecret != "" {
			hasADC = true
		}
	}

	tm.mu.Lock()
	defer tm.mu.Unlock()

	if hasGeminiFile {
		tm.accessToken = oauthCreds.AccessToken
		tm.refreshToken = oauthCreds.RefreshToken
		tm.source = geminiPath

		var expiresAt time.Time
		if oauthCreds.ExpiryDate > 0 {
			if oauthCreds.ExpiryDate > 1e12 {
				expiresAt = time.UnixMilli(oauthCreds.ExpiryDate)
			} else {
				expiresAt = time.Unix(oauthCreds.ExpiryDate, 0)
			}
		}
		if expiresAt.IsZero() || time.Until(expiresAt) <= 0 {
			tm.expiresAt = time.Now().Add(-1 * time.Second)
		} else {
			tm.expiresAt = expiresAt
		}
	} else if hasADC && adc.RefreshToken != "" {
		// Use gcloud ADC as fallback token source
		tm.refreshToken = adc.RefreshToken
		tm.source = adcPath
		tm.expiresAt = time.Now().Add(-1 * time.Second) // trigger immediate refresh
	} else {
		return fmt.Errorf("no gemini credentials found (tried %s and %s)", geminiOAuthFile, gcloudADCFile)
	}

	// Client ID/Secret: if tokens came from Gemini CLI file, use Gemini CLI credentials;
	// if tokens came from gcloud ADC, use ADC's own credentials.
	if hasGeminiFile {
		tm.clientID = envOrDefault("GEMINI_CLIENT_ID", defaultGeminiCLIClientID)
		tm.clientSecret = envOrDefault("GEMINI_CLIENT_SECRET", defaultGeminiCLIClientSecret)
	} else if hasADC {
		tm.clientID = adc.ClientID
		tm.clientSecret = adc.ClientSecret
	}
	tm.tokenURI = geminiTokenURL
	tm.lastError = nil
	return nil
}

func (tm *GeminiTokenManager) refresh(ctx context.Context) error {
	_, err, _ := tm.sfGroup.Do("refresh", func() (interface{}, error) {
		tm.mu.RLock()
		refreshToken := tm.refreshToken
		clientID := tm.clientID
		clientSecret := tm.clientSecret
		tokenURI := tm.tokenURI
		tm.mu.RUnlock()

		if refreshToken == "" {
			return nil, fmt.Errorf("no refresh token available")
		}
		if clientID == "" || clientSecret == "" {
			return nil, fmt.Errorf("no client_id/client_secret in gemini credentials")
		}
		if tokenURI == "" {
			tokenURI = geminiTokenURL
		}

		form := url.Values{
			"grant_type":    {"refresh_token"},
			"refresh_token": {refreshToken},
			"client_id":     {clientID},
			"client_secret": {clientSecret},
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURI, nil)
		if err != nil {
			return nil, err
		}
		req.URL.RawQuery = form.Encode()
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Body = io.NopCloser(nil)

		// Use PostForm style
		resp, err := tm.httpClient.PostForm(tokenURI, form)
		if err != nil {
			tm.mu.Lock()
			tm.lastError = err
			tm.mu.Unlock()
			return nil, fmt.Errorf("gemini refresh request failed: %w", err)
		}
		defer resp.Body.Close()

		respBody, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			errMsg := fmt.Errorf("gemini refresh failed (status %d): %s", resp.StatusCode, string(respBody))
			tm.mu.Lock()
			tm.lastError = errMsg
			tm.mu.Unlock()
			return nil, errMsg
		}

		var result struct {
			AccessToken string `json:"access_token"`
			ExpiresIn   int    `json:"expires_in"`
		}
		if err := json.Unmarshal(respBody, &result); err != nil {
			return nil, fmt.Errorf("parse gemini refresh response: %w", err)
		}

		if result.AccessToken == "" {
			return nil, fmt.Errorf("gemini refresh returned empty access token")
		}

		tm.mu.Lock()
		tm.accessToken = result.AccessToken
		if result.ExpiresIn > 0 {
			tm.expiresAt = time.Now().Add(time.Duration(result.ExpiresIn) * time.Second)
		} else {
			tm.expiresAt = time.Now().Add(1 * time.Hour)
		}
		tm.lastError = nil
		tm.mu.Unlock()

		log.Infof("gemini token refreshed, expires in %ds", result.ExpiresIn)
		return nil, nil
	})
	return err
}

func (tm *GeminiTokenManager) refreshLoop() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		tm.mu.RLock()
		expiresAt := tm.expiresAt
		hasRefresh := tm.refreshToken != ""
		tm.mu.RUnlock()

		if !hasRefresh {
			continue
		}
		if time.Until(expiresAt) < geminiRefreshMargin {
			log.Info("gemini token approaching expiry, refreshing...")
			if err := tm.refresh(context.Background()); err != nil {
				log.Errorf("gemini background refresh failed: %v", err)
				// Try reloading from file (user may have re-logged in via gemini CLI)
				log.Info("reloading gemini token from file...")
				if err2 := tm.loadFromFile(); err2 != nil {
					log.Errorf("gemini file reload also failed: %v", err2)
				} else {
					log.Info("gemini token reloaded from file, retrying refresh...")
					if err3 := tm.refresh(context.Background()); err3 != nil {
						log.Errorf("gemini refresh after reload also failed: %v", err3)
					}
				}
			}
		}
	}
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
