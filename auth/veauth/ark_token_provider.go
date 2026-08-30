// Copyright (c) 2025 Beijing Volcano Engine Technology Co., Ltd. and/or its affiliates.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package veauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/volcengine/veadk-go/common"
	"github.com/volcengine/veadk-go/integrations/ve_sign"
	"github.com/volcengine/veadk-go/log"
	"golang.org/x/sync/singleflight"
)

const (
	defaultArkControlPlaneEndpoint = "https://open.volcengineapi.com"
	defaultArkProjectName          = "default"
	defaultArkTokenTimeout         = 10 * time.Second
	defaultArkTokenMaxAttempts     = 3
)

// ArkTokenProviderConfig configures a lazy, process-local ARK API key provider.
// It is intentionally independent of MODEL_AGENT_API_KEY so a server can be
// constructed and start listening before STS credentials are exchanged.
type ArkTokenProviderConfig struct {
	Region         string
	CredentialPath string
	Endpoint       string
	ProjectName    string
	HTTPClient     *http.Client
	RequestTimeout time.Duration
	MaxAttempts    int
	InitialBackoff time.Duration
}

// ArkTokenProvider exchanges VeFaaS IAM credentials for an ARK API key on the
// first model request. The API key is cached in memory and concurrent callers
// share a single exchange.
type ArkTokenProvider struct {
	config ArkTokenProviderConfig

	mu     sync.RWMutex
	apiKey string
	group  singleflight.Group
}

func NewArkTokenProvider(config ArkTokenProviderConfig) *ArkTokenProvider {
	if strings.TrimSpace(config.Region) == "" {
		config.Region = common.DEFAULT_MODEL_REGION
	}
	if strings.TrimSpace(config.CredentialPath) == "" {
		config.CredentialPath = common.VEFAAS_IAM_CRIDENTIAL_PATH
	}
	if strings.TrimSpace(config.Endpoint) == "" {
		config.Endpoint = defaultArkControlPlaneEndpoint
	}
	if strings.TrimSpace(config.ProjectName) == "" {
		config.ProjectName = defaultArkProjectName
	}
	if config.RequestTimeout <= 0 {
		config.RequestTimeout = defaultArkTokenTimeout
	}
	if config.MaxAttempts <= 0 {
		config.MaxAttempts = defaultArkTokenMaxAttempts
	}
	if config.InitialBackoff <= 0 {
		config.InitialBackoff = 100 * time.Millisecond
	}
	if config.HTTPClient == nil {
		config.HTTPClient = &http.Client{Timeout: config.RequestTimeout}
	}
	return &ArkTokenProvider{config: config}
}

// APIKey returns a cached API key or lazily exchanges the current STS
// credential. Waiting callers may cancel independently through ctx.
func (p *ArkTokenProvider) APIKey(ctx context.Context) (string, error) {
	if key := p.cachedAPIKey(); key != "" {
		return key, nil
	}

	result := p.group.DoChan("ark-api-key", func() (any, error) {
		if key := p.cachedAPIKey(); key != "" {
			return key, nil
		}
		// Keep the shared exchange independent from the first waiter. Each
		// waiter may cancel through its own context, while the exchange itself
		// remains bounded by request timeouts and the retry count.
		key, err := p.fetchWithRetry(context.Background())
		if err != nil {
			return "", err
		}
		p.mu.Lock()
		p.apiKey = key
		p.mu.Unlock()
		return key, nil
	})

	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case res := <-result:
		if res.Err != nil {
			return "", res.Err
		}
		key, ok := res.Val.(string)
		if !ok || key == "" {
			return "", errors.New("ark token provider returned an empty API key")
		}
		return key, nil
	}
}

// Invalidate clears the in-memory key. The next call re-reads the credential
// file, which allows callers to recover after credential or API-key rotation.
func (p *ArkTokenProvider) Invalidate() {
	p.mu.Lock()
	p.apiKey = ""
	p.mu.Unlock()
}

func (p *ArkTokenProvider) cachedAPIKey() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.apiKey
}

func (p *ArkTokenProvider) fetchWithRetry(ctx context.Context) (string, error) {
	var lastErr error
	for attempt := 1; attempt <= p.config.MaxAttempts; attempt++ {
		requestCtx, cancel := context.WithTimeout(ctx, p.config.RequestTimeout)
		key, err := p.fetch(requestCtx)
		cancel()
		if err == nil {
			return key, nil
		}
		lastErr = err
		if attempt == p.config.MaxAttempts || ctx.Err() != nil {
			break
		}

		delay := p.config.InitialBackoff * time.Duration(1<<(attempt-1))
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return "", ctx.Err()
		case <-timer.C:
		}
	}
	return "", fmt.Errorf("failed to fetch ARK API key after %d attempt(s): %w", p.config.MaxAttempts, lastErr)
}

func (p *ArkTokenProvider) fetch(ctx context.Context) (string, error) {
	accessKey := strings.TrimSpace(os.Getenv(common.VOLCENGINE_ACCESS_KEY))
	secretKey := strings.TrimSpace(os.Getenv(common.VOLCENGINE_SECRET_KEY))
	sessionToken := strings.TrimSpace(os.Getenv("VOLCENGINE_SESSION_TOKEN"))
	if sessionToken == "" {
		sessionToken = strings.TrimSpace(os.Getenv("VOLC_SESSIONTOKEN"))
	}
	if accessKey == "" || secretKey == "" {
		credential, err := GetCredentialFromVeFaaSIAM(p.config.CredentialPath)
		if err != nil {
			return "", fmt.Errorf("failed to read VeFaaS IAM credential: %w", err)
		}
		accessKey = credential.AccessKeyID
		secretKey = credential.SecretAccessKey
		sessionToken = credential.SessionToken
	}

	endpoint, err := url.Parse(p.config.Endpoint)
	if err != nil {
		return "", fmt.Errorf("invalid ARK control-plane endpoint: %w", err)
	}
	if endpoint.Scheme != "http" && endpoint.Scheme != "https" {
		return "", fmt.Errorf("invalid ARK control-plane endpoint scheme %q", endpoint.Scheme)
	}
	if endpoint.Host == "" || endpoint.Path != "" && endpoint.Path != "/" {
		return "", fmt.Errorf("invalid ARK control-plane endpoint %q", p.config.Endpoint)
	}

	header := map[string]string{}
	if sessionToken != "" {
		header["X-Security-Token"] = sessionToken
	}

	listRequest := ve_sign.VeRequest{
		AK: accessKey, SK: secretKey, Method: http.MethodPost,
		Scheme: endpoint.Scheme, Host: endpoint.Host, Path: "/",
		Service: "ark", Region: p.config.Region,
		Action: "ListApiKeys", Version: "2024-01-01", Header: header,
		Body: map[string]any{
			"ProjectName": p.config.ProjectName,
			"Filter":      map[string]any{"AllowAll": true},
			"PageNumber":  1,
			"PageSize":    100,
		},
	}
	listBody, err := listRequest.DoRequestWithContext(ctx, p.config.HTTPClient)
	if err != nil {
		return "", fmt.Errorf("failed to list API keys: %w", err)
	}
	var listResponse listApiKeysResponse
	if err := json.Unmarshal(listBody, &listResponse); err != nil {
		return "", fmt.Errorf("failed to decode API key list: %w", err)
	}
	if len(listResponse.Result.Items) == 0 {
		return "", errors.New("ARK API key list is empty")
	}

	getRequest := ve_sign.VeRequest{
		AK: accessKey, SK: secretKey, Method: http.MethodPost,
		Scheme: endpoint.Scheme, Host: endpoint.Host, Path: "/",
		Service: "ark", Region: p.config.Region,
		Action: "GetRawApiKey", Version: "2024-01-01", Header: header,
		Body: map[string]any{
			"Id":          listResponse.Result.Items[0].ID,
			"ProjectName": p.config.ProjectName,
		},
	}
	getBody, err := getRequest.DoRequestWithContext(ctx, p.config.HTTPClient)
	if err != nil {
		return "", fmt.Errorf("failed to get raw API key: %w", err)
	}
	var getResponse getRawApiKeyResponse
	if err := json.Unmarshal(getBody, &getResponse); err != nil {
		return "", fmt.Errorf("failed to decode raw API key response: %w", err)
	}
	if getResponse.Result.ApiKey == "" {
		return "", errors.New("ARK API key is empty")
	}

	log.Info("Successfully fetched and cached ARK API Key.")
	return getResponse.Result.ApiKey, nil
}
