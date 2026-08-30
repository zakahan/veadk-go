// Copyright (c) 2025 Beijing Volcano Engine Technology Co., Ltd. and/or its affiliates.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0

package veauth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/volcengine/veadk-go/common"
)

func TestArkTokenProviderCachesAndCollapsesConcurrentFetches(t *testing.T) {
	t.Setenv(common.VOLCENGINE_ACCESS_KEY, "")
	t.Setenv(common.VOLCENGINE_SECRET_KEY, "")

	var listCalls atomic.Int32
	var getCalls atomic.Int32
	var securityToken atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		securityToken.Store(r.Header.Get("X-Security-Token"))
		switch r.URL.Query().Get("Action") {
		case "ListApiKeys":
			listCalls.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Result": map[string]any{"Items": []map[string]any{{"Id": 1, "Name": "default"}}},
			})
		case "GetRawApiKey":
			getCalls.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Result": map[string]any{"ApiKey": "ark-test-key"},
			})
		default:
			http.Error(w, "unexpected action", http.StatusBadRequest)
		}
	}))
	defer server.Close()

	credentialPath := writeTempCredential(t, t.TempDir(), "ak", "sk", "session-token")
	provider := NewArkTokenProvider(ArkTokenProviderConfig{
		CredentialPath: credentialPath,
		Endpoint:       server.URL,
		HTTPClient:     server.Client(),
		MaxAttempts:    1,
	})

	const callers = 20
	var wg sync.WaitGroup
	errors := make(chan error, callers)
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			key, err := provider.APIKey(context.Background())
			if err != nil {
				errors <- err
				return
			}
			if key != "ark-test-key" {
				t.Errorf("APIKey() = %q, want ark-test-key", key)
			}
		}()
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		t.Fatalf("APIKey() error = %v", err)
	}

	if got := listCalls.Load(); got != 1 {
		t.Fatalf("ListApiKeys calls = %d, want 1", got)
	}
	if got := getCalls.Load(); got != 1 {
		t.Fatalf("GetRawApiKey calls = %d, want 1", got)
	}
	if got, _ := securityToken.Load().(string); got != "session-token" {
		t.Fatalf("X-Security-Token = %q, want session-token", got)
	}
}

func TestArkTokenProviderInvalidateReReadsCredential(t *testing.T) {
	t.Setenv(common.VOLCENGINE_ACCESS_KEY, "")
	t.Setenv(common.VOLCENGINE_SECRET_KEY, "")

	var mu sync.Mutex
	var tokens []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		tokens = append(tokens, r.Header.Get("X-Security-Token"))
		mu.Unlock()
		if r.URL.Query().Get("Action") == "ListApiKeys" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Result": map[string]any{"Items": []map[string]any{{"Id": 1}}},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"Result": map[string]any{"ApiKey": "ark-test-key"},
		})
	}))
	defer server.Close()

	dir := t.TempDir()
	credentialPath := writeTempCredential(t, dir, "ak", "sk", "session-one")
	provider := NewArkTokenProvider(ArkTokenProviderConfig{
		CredentialPath: credentialPath,
		Endpoint:       server.URL,
		HTTPClient:     server.Client(),
		MaxAttempts:    1,
	})
	if _, err := provider.APIKey(context.Background()); err != nil {
		t.Fatalf("first APIKey() error = %v", err)
	}
	if err := os.WriteFile(credentialPath, []byte(`{"access_key_id":"ak","secret_access_key":"sk","session_token":"session-two"}`), 0o600); err != nil {
		t.Fatalf("rewrite credential: %v", err)
	}
	provider.Invalidate()
	if _, err := provider.APIKey(context.Background()); err != nil {
		t.Fatalf("second APIKey() error = %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(tokens) != 4 {
		t.Fatalf("request count = %d, want 4", len(tokens))
	}
	if tokens[0] != "session-one" || tokens[2] != "session-two" {
		t.Fatalf("security tokens = %v, want session-one then session-two", tokens)
	}
}

func TestArkTokenProviderHonorsRequestTimeout(t *testing.T) {
	t.Setenv(common.VOLCENGINE_ACCESS_KEY, "")
	t.Setenv(common.VOLCENGINE_SECRET_KEY, "")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		_ = json.NewEncoder(w).Encode(map[string]any{})
	}))
	defer server.Close()

	provider := NewArkTokenProvider(ArkTokenProviderConfig{
		CredentialPath: writeTempCredential(t, t.TempDir(), "ak", "sk", "session-token"),
		Endpoint:       server.URL,
		HTTPClient:     server.Client(),
		RequestTimeout: 20 * time.Millisecond,
		MaxAttempts:    1,
	})
	started := time.Now()
	if _, err := provider.APIKey(context.Background()); err == nil {
		t.Fatal("APIKey() error = nil, want timeout")
	}
	if elapsed := time.Since(started); elapsed > 150*time.Millisecond {
		t.Fatalf("APIKey() elapsed = %v, want bounded timeout", elapsed)
	}
}

func TestArkTokenProviderFirstCallerCancellationDoesNotCancelSharedFetch(t *testing.T) {
	t.Setenv(common.VOLCENGINE_ACCESS_KEY, "")
	t.Setenv(common.VOLCENGINE_SECRET_KEY, "")

	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		time.Sleep(40 * time.Millisecond)
		if r.URL.Query().Get("Action") == "ListApiKeys" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Result": map[string]any{"Items": []map[string]any{{"Id": 1}}},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"Result": map[string]any{"ApiKey": "shared-key"},
		})
	}))
	defer server.Close()

	provider := NewArkTokenProvider(ArkTokenProviderConfig{
		CredentialPath: writeTempCredential(t, t.TempDir(), "ak", "sk", "token"),
		Endpoint:       server.URL,
		HTTPClient:     server.Client(),
		RequestTimeout: time.Second,
		MaxAttempts:    1,
	})
	firstCtx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	firstResult := make(chan error, 1)
	go func() {
		_, err := provider.APIKey(firstCtx)
		firstResult <- err
	}()
	time.Sleep(5 * time.Millisecond)

	key, err := provider.APIKey(context.Background())
	if err != nil {
		t.Fatalf("second APIKey() error = %v", err)
	}
	if key != "shared-key" {
		t.Fatalf("second APIKey() = %q, want shared-key", key)
	}
	if err := <-firstResult; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first APIKey() error = %v, want deadline exceeded", err)
	}
	if got, want := requests.Load(), int32(2); got != want {
		t.Fatalf("request count = %d, want %d", got, want)
	}
}

func TestArkTokenProviderUsesEnvironmentSessionTokenWithEnvironmentKeys(t *testing.T) {
	t.Setenv(common.VOLCENGINE_ACCESS_KEY, "environment-ak")
	t.Setenv(common.VOLCENGINE_SECRET_KEY, "environment-sk")
	t.Setenv("VOLCENGINE_SESSION_TOKEN", "environment-session-token")

	var securityTokens []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		securityTokens = append(securityTokens, r.Header.Get("X-Security-Token"))
		if r.URL.Query().Get("Action") == "ListApiKeys" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Result": map[string]any{"Items": []map[string]any{{"Id": 1}}},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"Result": map[string]any{"ApiKey": "environment-key"},
		})
	}))
	defer server.Close()

	provider := NewArkTokenProvider(ArkTokenProviderConfig{
		CredentialPath: filepath.Join(t.TempDir(), "must-not-be-read"),
		Endpoint:       server.URL,
		HTTPClient:     server.Client(),
		MaxAttempts:    1,
	})
	if _, err := provider.APIKey(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, want := securityTokens, []string{"environment-session-token", "environment-session-token"}; !slices.Equal(got, want) {
		t.Fatalf("security tokens = %v, want %v", got, want)
	}
}
