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
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestArkTokenProviderIsLazyCachesAndCollapsesConcurrentFetches(t *testing.T) {
	var credentialCalls atomic.Int32
	source := CredentialSourceFunc(func(context.Context) (VeIAMCredential, error) {
		credentialCalls.Add(1)
		return VeIAMCredential{AccessKeyID: "ak", SecretAccessKey: "sk", SessionToken: "session-token"}, nil
	})

	var listCalls atomic.Int32
	var getCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Security-Token"); got != "session-token" {
			t.Errorf("X-Security-Token = %q, want session-token", got)
		}
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

	provider := NewArkTokenProvider(ArkTokenProviderConfig{
		CredentialSource: source,
		Endpoint:         server.URL,
		HTTPClient:       server.Client(),
		MaxAttempts:      1,
	})
	if credentialCalls.Load() != 0 || listCalls.Load() != 0 {
		t.Fatal("provider construction accessed credentials or network")
	}

	const callers = 32
	var wait sync.WaitGroup
	errorsCh := make(chan error, callers)
	for range callers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			key, err := provider.APIKey(context.Background())
			if err != nil {
				errorsCh <- err
				return
			}
			if key != "ark-test-key" {
				errorsCh <- errors.New("unexpected API key")
			}
		}()
	}
	wait.Wait()
	close(errorsCh)
	for err := range errorsCh {
		t.Fatal(err)
	}

	if got := credentialCalls.Load(); got != 1 {
		t.Fatalf("credential calls = %d, want 1", got)
	}
	if got := listCalls.Load(); got != 1 {
		t.Fatalf("ListApiKeys calls = %d, want 1", got)
	}
	if got := getCalls.Load(); got != 1 {
		t.Fatalf("GetRawApiKey calls = %d, want 1", got)
	}
}

func TestArkTokenProviderInvalidateRefreshesKey(t *testing.T) {
	var getCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("Action") == "ListApiKeys" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Result": map[string]any{"Items": []map[string]any{{"Id": 1}}},
			})
			return
		}
		call := getCalls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"Result": map[string]any{"ApiKey": "key-" + string(rune('0'+call))},
		})
	}))
	defer server.Close()

	provider := newTestArkTokenProvider(server)
	first, err := provider.APIKey(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	provider.Invalidate()
	second, err := provider.APIKey(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first != "key-1" || second != "key-2" {
		t.Fatalf("keys = %q, %q, want key-1, key-2", first, second)
	}
}

func TestArkTokenProviderInvalidateDoesNotRestoreInFlightOldKey(t *testing.T) {
	firstSourceStarted := make(chan struct{})
	releaseFirstSource := make(chan struct{})
	var sourceCalls atomic.Int32
	source := CredentialSourceFunc(func(context.Context) (VeIAMCredential, error) {
		call := sourceCalls.Add(1)
		if call == 1 {
			close(firstSourceStarted)
			<-releaseFirstSource
			return VeIAMCredential{AccessKeyID: "ak", SecretAccessKey: "sk", SessionToken: "old"}, nil
		}
		return VeIAMCredential{AccessKeyID: "ak", SecretAccessKey: "sk", SessionToken: "new"}, nil
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("Action") == "ListApiKeys" {
			_ = json.NewEncoder(w).Encode(map[string]any{"Result": map[string]any{"Items": []map[string]any{{"Id": 1}}}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"Result": map[string]any{"ApiKey": r.Header.Get("X-Security-Token") + "-key"},
		})
	}))
	defer server.Close()

	provider := NewArkTokenProvider(ArkTokenProviderConfig{
		CredentialSource: source, Endpoint: server.URL, HTTPClient: server.Client(), MaxAttempts: 1,
	})
	firstResult := make(chan string, 1)
	firstError := make(chan error, 1)
	go func() {
		key, err := provider.APIKey(context.Background())
		firstResult <- key
		firstError <- err
	}()
	<-firstSourceStarted
	provider.Invalidate()

	second, err := provider.APIKey(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	close(releaseFirstSource)
	first := <-firstResult
	if err := <-firstError; err != nil {
		t.Fatal(err)
	}
	if first != "new-key" || second != "new-key" {
		t.Fatalf("keys = %q, %q, want new-key", first, second)
	}
	if got := sourceCalls.Load(); got != 2 {
		t.Fatalf("credential source calls = %d, want 2", got)
	}
}

func TestArkTokenProviderRetriesTransientFailureButNotPermissionFailure(t *testing.T) {
	t.Run("transient", func(t *testing.T) {
		var calls atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if calls.Add(1) == 1 {
				http.Error(w, "temporary", http.StatusInternalServerError)
				return
			}
			if r.URL.Query().Get("Action") == "ListApiKeys" {
				_ = json.NewEncoder(w).Encode(map[string]any{"Result": map[string]any{"Items": []map[string]any{{"Id": 1}}}})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"Result": map[string]any{"ApiKey": "key"}})
		}))
		defer server.Close()

		provider := NewArkTokenProvider(ArkTokenProviderConfig{
			CredentialSource: staticTestCredentialSource(), Endpoint: server.URL,
			HTTPClient: server.Client(), MaxAttempts: 2, InitialBackoff: time.Millisecond,
		})
		if key, err := provider.APIKey(context.Background()); err != nil || key != "key" {
			t.Fatalf("APIKey() = %q, %v", key, err)
		}
		if got := calls.Load(); got != 3 {
			t.Fatalf("requests = %d, want 3", got)
		}
	})

	t.Run("permission", func(t *testing.T) {
		var calls atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			http.Error(w, "denied", http.StatusForbidden)
		}))
		defer server.Close()

		provider := NewArkTokenProvider(ArkTokenProviderConfig{
			CredentialSource: staticTestCredentialSource(), Endpoint: server.URL,
			HTTPClient: server.Client(), MaxAttempts: 3, InitialBackoff: time.Millisecond,
		})
		if _, err := provider.APIKey(context.Background()); err == nil {
			t.Fatal("APIKey() error = nil")
		}
		if got := calls.Load(); got != 1 {
			t.Fatalf("requests = %d, want 1", got)
		}
	})
}

func TestArkTokenProviderWaiterCancellationDoesNotCancelSharedFetch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(30 * time.Millisecond)
		if r.URL.Query().Get("Action") == "ListApiKeys" {
			_ = json.NewEncoder(w).Encode(map[string]any{"Result": map[string]any{"Items": []map[string]any{{"Id": 1}}}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"Result": map[string]any{"ApiKey": "shared-key"}})
	}))
	defer server.Close()

	provider := newTestArkTokenProvider(server)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	first := make(chan error, 1)
	go func() {
		_, err := provider.APIKey(ctx)
		first <- err
	}()
	time.Sleep(time.Millisecond)

	key, err := provider.APIKey(context.Background())
	if err != nil || key != "shared-key" {
		t.Fatalf("second APIKey() = %q, %v", key, err)
	}
	if err := <-first; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first APIKey() error = %v, want deadline exceeded", err)
	}
}

func TestArkTokenProviderBoundsControlPlaneAttempt(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		_ = json.NewEncoder(w).Encode(map[string]any{})
	}))
	defer server.Close()

	provider := NewArkTokenProvider(ArkTokenProviderConfig{
		CredentialSource: staticTestCredentialSource(), Endpoint: server.URL,
		HTTPClient: server.Client(), RequestTimeout: 10 * time.Millisecond, MaxAttempts: 1,
	})
	started := time.Now()
	if _, err := provider.APIKey(context.Background()); err == nil {
		t.Fatal("APIKey() error = nil, want timeout")
	}
	if elapsed := time.Since(started); elapsed > 200*time.Millisecond {
		t.Fatalf("APIKey() elapsed = %v, want bounded attempt", elapsed)
	}
}

func newTestArkTokenProvider(server *httptest.Server) *ArkTokenProvider {
	return NewArkTokenProvider(ArkTokenProviderConfig{
		CredentialSource: staticTestCredentialSource(),
		Endpoint:         server.URL,
		HTTPClient:       server.Client(),
		MaxAttempts:      1,
	})
}

func staticTestCredentialSource() CredentialSource {
	return CredentialSourceFunc(func(context.Context) (VeIAMCredential, error) {
		return VeIAMCredential{AccessKeyID: "ak", SecretAccessKey: "sk", SessionToken: "token"}, nil
	})
}
