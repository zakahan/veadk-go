// Copyright (c) 2026 Beijing Volcano Engine Technology Co., Ltd. and/or its affiliates.
// SPDX-License-Identifier: Apache-2.0

package agentkit

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"maps"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"google.golang.org/adk/v2/session"
)

// AIOLifecycleConfig configures one owner of AIO sandbox sessions. A single
// owner should be started per process; a shared lease is needed across replicas.
type AIOLifecycleConfig struct {
	SessionService session.Service
	SkillSpaceIDs  []string
	// IdleTimeout controls time since the last completed request before pause.
	// Zero reads VEADK_AIO_IDLE_TIMEOUT, then defaults to five minutes.
	IdleTimeout time.Duration
	// PausedRetention controls time since confirmed pause before deletion.
	// Zero reads VEADK_AIO_PAUSED_RETENTION, then defaults to 24 hours.
	PausedRetention time.Duration
	// ScanInterval controls how often the background worker checks sessions.
	// Zero reads VEADK_AIO_SCAN_INTERVAL, then defaults to 30 seconds.
	ScanInterval time.Duration
	Client       *Client // Optional, primarily for embedding and tests.
}

type aioTrackedSession struct {
	app, user, id string
	lastActivity  time.Time
	pausedAt      time.Time
	deleted       bool
	mu            sync.Mutex
}

// AIOLifecycle manages physical AgentKit sessions without owning ADK history.
// Its registry is process-local. An embedding with multiple replicas must use
// one scheduler or a distributed lease for each logical ADK session.
type AIOLifecycle struct {
	cfg     AIOLifecycleConfig
	mu      sync.Mutex
	tracked map[string]*aioTrackedSession
	start   sync.Once
}

func (m *AIOLifecycle) SkillSpaceIDs() []string {
	if m == nil {
		return nil
	}
	return append([]string(nil), m.cfg.SkillSpaceIDs...)
}

func (m *AIOLifecycle) SessionService() session.Service {
	if m == nil {
		return nil
	}
	return m.cfg.SessionService
}

var ErrAIOSessionExists = errors.New("ADK session already exists")

func NewAIOLifecycle(cfg AIOLifecycleConfig) (*AIOLifecycle, error) {
	if cfg.SessionService == nil {
		return nil, errors.New("AIO lifecycle requires SessionService")
	}
	if cfg.IdleTimeout < 0 || cfg.PausedRetention < 0 || cfg.ScanInterval < 0 {
		return nil, errors.New("AIO lifecycle durations cannot be negative")
	}
	var err error
	if cfg.IdleTimeout, err = aioDuration(cfg.IdleTimeout, "VEADK_AIO_IDLE_TIMEOUT", 5*time.Minute); err != nil {
		return nil, err
	}
	if cfg.PausedRetention, err = aioDuration(cfg.PausedRetention, "VEADK_AIO_PAUSED_RETENTION", 24*time.Hour); err != nil {
		return nil, err
	}
	if cfg.ScanInterval, err = aioDuration(cfg.ScanInterval, "VEADK_AIO_SCAN_INTERVAL", 30*time.Second); err != nil {
		return nil, err
	}
	cfg.SkillSpaceIDs = append([]string(nil), cfg.SkillSpaceIDs...)
	return &AIOLifecycle{cfg: cfg, tracked: make(map[string]*aioTrackedSession)}, nil
}

func aioDuration(explicit time.Duration, env string, fallback time.Duration) (time.Duration, error) {
	if explicit > 0 {
		return explicit, nil
	}
	value := strings.TrimSpace(os.Getenv(env))
	if value == "" {
		return fallback, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 {
		return 0, fmt.Errorf("%s must be a positive Go duration, such as 5m or 24h", env)
	}
	return duration, nil
}

func (m *AIOLifecycle) client() (*Client, error) {
	if m.cfg.Client != nil {
		return m.cfg.Client, nil
	}
	return NewClient(nil)
}

func (m *AIOLifecycle) sessionTTLSeconds() int {
	seconds := int((m.cfg.IdleTimeout + 5*time.Minute + time.Second - 1) / time.Second)
	if seconds < aioSessionTTL {
		return aioSessionTTL
	}
	return seconds
}

func aioTrackKey(app, user, id string) string {
	return app + "\x00" + user + "\x00" + id
}

func (m *AIOLifecycle) track(app, user, id string) *aioTrackedSession {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := aioTrackKey(app, user, id)
	if entry := m.tracked[key]; entry != nil {
		return entry
	}
	entry := &aioTrackedSession{app: app, user: user, id: id, lastActivity: time.Now()}
	m.tracked[key] = entry
	return entry
}

// Create creates an ADK session after a ready sandbox has been identified. The
// binding is included in CreateRequest.State, so database SessionServices can
// persist it in the same transaction as the ADK session.
func (m *AIOLifecycle) Create(ctx context.Context, app, user, id string, initialState map[string]any) (session.Session, error) {
	if app == "" || user == "" {
		return nil, errors.New("app and user are required")
	}
	if _, ok := initialState[AIOSandboxStateKey]; ok {
		return nil, errors.New("sandbox binding is server-owned")
	}
	if id == "" {
		id = uuid.NewString()
	}
	entry := m.track(app, user, id)
	entry.mu.Lock()
	defer entry.mu.Unlock()
	listed, err := m.cfg.SessionService.List(ctx, &session.ListRequest{AppName: app, UserID: user})
	if err != nil {
		return nil, fmt.Errorf("check ADK session ID: %w", err)
	}
	for _, existing := range listed.Sessions {
		if existing.ID() == id {
			return nil, ErrAIOSessionExists
		}
	}
	client, err := m.client()
	if err != nil {
		return nil, err
	}
	state := &aioMapState{values: maps.Clone(initialState)}
	if state.values == nil {
		state.values = map[string]any{}
	}
	if _, err = ensureAIOBindingWithSkillsTTL(ctx, client, state, app, user, id, m.cfg.SkillSpaceIDs, m.sessionTTLSeconds()); err != nil {
		return nil, err
	}
	created, err := m.cfg.SessionService.Create(ctx, &session.CreateRequest{AppName: app, UserID: user, SessionID: id, State: state.values})
	if err != nil {
		// Reconciliation on the next request uses the deterministic identity;
		// do not delete a sandbox whose creation ownership may be ambiguous.
		return nil, fmt.Errorf("create ADK session: %w", err)
	}
	entry.lastActivity = time.Now()
	return created.Session, nil
}

// EnsureReady validates and resumes or recreates the physical sandbox.
func (m *AIOLifecycle) EnsureReady(ctx context.Context, app, user, id string) (session.Session, error) {
	entry := m.track(app, user, id)
	entry.mu.Lock()
	defer entry.mu.Unlock()
	return m.ensureReadyLocked(ctx, entry)
}

func (m *AIOLifecycle) ensureReadyLocked(ctx context.Context, entry *aioTrackedSession) (session.Session, error) {
	got, err := m.cfg.SessionService.Get(ctx, &session.GetRequest{AppName: entry.app, UserID: entry.user, SessionID: entry.id})
	if err != nil {
		return nil, err
	}
	previous, err := bindingFromState(got.Session.State())
	if err != nil {
		return nil, err
	}
	client, err := m.client()
	if err != nil {
		return nil, err
	}
	next, err := ensureAIOBindingWithSkillsTTL(ctx, client, got.Session.State(), entry.app, entry.user, entry.id, m.cfg.SkillSpaceIDs, m.sessionTTLSeconds())
	if err != nil {
		return nil, err
	}
	if previous == nil || *previous != *next {
		// State().Set alone is not durable for ADK database services.
		event := &session.Event{Author: "veadk_lifecycle", Timestamp: time.Now()}
		event.Actions.StateDelta = map[string]any{AIOSandboxStateKey: bindingValue(*next)}
		if err := m.cfg.SessionService.AppendEvent(ctx, got.Session, event); err != nil {
			return nil, fmt.Errorf("persist AIO sandbox binding: %w", err)
		}
	}
	entry.pausedAt = time.Time{}
	entry.deleted = false
	entry.lastActivity = time.Now()
	return got.Session, nil
}

// WithSession serializes one invocation with pause, resume and deletion.
// Callers must keep the callback running until the SSE stream has ended.
func (m *AIOLifecycle) WithSession(ctx context.Context, app, user, id string, run func() error) error {
	entry := m.track(app, user, id)
	if !entry.mu.TryLock() {
		return ErrAIOSessionBusy
	}
	defer entry.mu.Unlock()
	if _, err := m.ensureReadyLocked(ctx, entry); err != nil {
		return err
	}
	defer func() { entry.lastActivity = time.Now() }()
	return run()
}

var ErrAIOSessionBusy = errors.New("AIO session is busy")

// Delete removes the physical sandbox before removing the ADK session.
func (m *AIOLifecycle) Delete(ctx context.Context, app, user, id string) error {
	entry := m.track(app, user, id)
	if !entry.mu.TryLock() {
		return ErrAIOSessionBusy
	}
	defer entry.mu.Unlock()
	got, err := m.cfg.SessionService.Get(ctx, &session.GetRequest{AppName: app, UserID: user, SessionID: id})
	if err != nil {
		return err
	}
	client, err := m.client()
	if err != nil {
		return err
	}
	if err := DeleteAIOBinding(ctx, client, got.Session.State(), app, user, id); err != nil {
		return err
	}
	if err := m.cfg.SessionService.Delete(ctx, &session.DeleteRequest{AppName: app, UserID: user, SessionID: id}); err != nil {
		return err
	}
	m.mu.Lock()
	delete(m.tracked, aioTrackKey(app, user, id))
	m.mu.Unlock()
	return nil
}

// Start runs the idle lifecycle worker until ctx is cancelled. The registry
// contains sessions created or used by this owner since process startup.
func (m *AIOLifecycle) Start(ctx context.Context) {
	m.start.Do(func() {
		go func() {
			ticker := time.NewTicker(m.cfg.ScanInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					m.Sweep(ctx)
				}
			}
		}()
	})
}

// Sweep performs one pass. Failed or ambiguous control-plane writes are
// revisited on the next pass after observing the actual session status.
func (m *AIOLifecycle) Sweep(ctx context.Context) {
	m.mu.Lock()
	entries := make([]*aioTrackedSession, 0, len(m.tracked))
	for _, entry := range m.tracked {
		entries = append(entries, entry)
	}
	m.mu.Unlock()
	for _, entry := range entries {
		if !entry.mu.TryLock() {
			continue
		}
		func() {
			defer entry.mu.Unlock()
			now := time.Now()
			if entry.pausedAt.IsZero() && now.Sub(entry.lastActivity) < m.cfg.IdleTimeout {
				return
			}
			if entry.deleted {
				return
			}
			if !entry.pausedAt.IsZero() && now.Sub(entry.pausedAt) < m.cfg.PausedRetention {
				return
			}
			got, err := m.cfg.SessionService.Get(ctx, &session.GetRequest{AppName: entry.app, UserID: entry.user, SessionID: entry.id})
			if err != nil {
				return
			}
			client, err := m.client()
			if err != nil {
				return
			}
			if entry.pausedAt.IsZero() {
				if err := PauseAIOBinding(ctx, client, got.Session.State(), entry.app, entry.user, entry.id); err == nil {
					entry.pausedAt = time.Now()
				}
				return
			}
			if err := DeleteAIOBinding(ctx, client, got.Session.State(), entry.app, entry.user, entry.id); err == nil {
				entry.pausedAt = time.Time{}
				entry.deleted = true
				entry.lastActivity = time.Now()
				// The old binding remains in ADK state until EnsureReady replaces
				// it. No old endpoint is used while it is absent.
			}
		}()
	}
}

type aioMapState struct{ values map[string]any }

func (s *aioMapState) Get(key string) (any, error) {
	v, ok := s.values[key]
	if !ok {
		return nil, session.ErrStateKeyNotExist
	}
	return v, nil
}
func (s *aioMapState) Set(key string, value any) error { s.values[key] = value; return nil }
func (s *aioMapState) All() iter.Seq2[string, any] {
	return func(yield func(string, any) bool) {
		for k, v := range s.values {
			if !yield(k, v) {
				return
			}
		}
	}
}
