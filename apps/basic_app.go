// Copyright (c) 2025 Beijing Volcano Engine Technology Co., Ltd. and/or its affiliates.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package apps

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/a2aproject/a2a-go/a2asrv"
	"github.com/gorilla/mux"
	"github.com/volcengine/veadk-go/log"
	"github.com/volcengine/veadk-go/observability"
	"google.golang.org/adk/agent"
	"google.golang.org/adk/artifact"
	"google.golang.org/adk/cmd/launcher/web"
	"google.golang.org/adk/memory"
	"google.golang.org/adk/plugin"
	"google.golang.org/adk/runner"
	"google.golang.org/adk/session"
	"google.golang.org/adk/telemetry"
)

type RunConfig struct {
	SessionService   session.Service
	ArtifactService  artifact.Service
	MemoryService    memory.Service
	AgentLoader      agent.Loader
	A2AOptions       []a2asrv.RequestHandlerOption
	PluginConfig     runner.PluginConfig
	TelemetryOptions []telemetry.Option
	// DisableObservability skips VeADK's default observability plugin and
	// telemetry shutdown hooks. It is intended for latency-sensitive embedders
	// that manage telemetry themselves. The default remains enabled.
	DisableObservability bool
}

func (cfg *RunConfig) AppendObservability() {
	if len(cfg.PluginConfig.Plugins) == 0 {
		cfg.PluginConfig = runner.PluginConfig{
			Plugins: []*plugin.Plugin{observability.NewPlugin()},
		}
	} else {
		observabilityPlugin := observability.NewPlugin()
		for _, p := range cfg.PluginConfig.Plugins {
			if p.Name() == observabilityPlugin.Name() {
				log.Info("Plugin already configured")
				return
			}
		}
		cfg.PluginConfig.Plugins = append(cfg.PluginConfig.Plugins, observabilityPlugin)
		log.Info("Plugin configured")
	}

	cfg.TelemetryOptions = append(cfg.TelemetryOptions, observability.ADKTelemetryOptions()...)
}

type ApiConfig struct {
	Host                 string
	Port                 int
	PublicURL            string
	WriteTimeout         time.Duration
	ReadTimeout          time.Duration
	IdleTimeout          time.Duration
	SEEWriteTimeout      time.Duration
	ShutdownTimeout      time.Duration
	ApiPathPrefix        string
	A2APath              string
	A2APublicPath        string
	AgentCardURLResolver func(*http.Request, *ApiConfig) string
	EnableSimpleAPI      bool
	EnableWebUI          bool
}

type BasicApp interface {
	Run(ctx context.Context, config *RunConfig) error
	SetupRouters(router *mux.Router, config *RunConfig) error
	GetApiConfig() *ApiConfig
	GetServerName() string
}

func DefaultApiConfig() *ApiConfig {
	return &ApiConfig{
		Host:            "",
		Port:            8000,
		WriteTimeout:    time.Second * 60,
		ReadTimeout:     time.Second * 60,
		IdleTimeout:     time.Second * 120,
		SEEWriteTimeout: time.Second * 300,
		ShutdownTimeout: time.Second * 30,
		ApiPathPrefix:   "", // set /api same as ADK-Go
		A2APath:         "/",
		A2APublicPath:   "/",
		EnableSimpleAPI: true,
		EnableWebUI:     true,
	}
}

func (a *ApiConfig) SetHost(host string) *ApiConfig {
	a.Host = host
	return a
}

func (a *ApiConfig) SetPort(port int) *ApiConfig {
	a.Port = port
	return a
}

func (a *ApiConfig) SetPublicURL(publicURL string) *ApiConfig {
	a.PublicURL = strings.TrimRight(publicURL, "/")
	return a
}

func (a *ApiConfig) SetWriteTimeout(t int64) *ApiConfig {
	a.WriteTimeout = time.Second * time.Duration(t)
	return a
}

func (a *ApiConfig) SetReadTimeout(t int64) *ApiConfig {
	a.ReadTimeout = time.Second * time.Duration(t)
	return a
}

func (a *ApiConfig) SetIdleTimeout(t int64) *ApiConfig {
	a.IdleTimeout = time.Second * time.Duration(t)
	return a
}

func (a *ApiConfig) SetSEEWriteTimeout(t int64) *ApiConfig {
	a.SEEWriteTimeout = time.Second * time.Duration(t)
	return a
}

func (a *ApiConfig) SetShutdownTimeout(t int64) *ApiConfig {
	a.ShutdownTimeout = time.Second * time.Duration(t)
	return a
}

func (a *ApiConfig) SetApiPathPrefix(p string) *ApiConfig {
	a.ApiPathPrefix = normalizePathPrefix(p)
	return a
}

func (a *ApiConfig) SetA2APath(p string) *ApiConfig {
	a.A2APath = normalizeRoutePath(p)
	return a
}

func (a *ApiConfig) SetA2APublicPath(p string) *ApiConfig {
	a.A2APublicPath = normalizeRoutePath(p)
	return a
}

func (a *ApiConfig) SetAgentCardURLResolver(resolver func(*http.Request, *ApiConfig) string) *ApiConfig {
	a.AgentCardURLResolver = resolver
	return a
}

func (a *ApiConfig) SetSimpleAPIEnabled(enabled bool) *ApiConfig {
	a.EnableSimpleAPI = enabled
	return a
}

func (a *ApiConfig) SetWebUIEnabled(enabled bool) *ApiConfig {
	a.EnableWebUI = enabled
	return a
}

func (a *ApiConfig) GetWebUrl() string {
	if a.PublicURL != "" {
		return strings.TrimRight(a.PublicURL, "/")
	}
	host := a.Host
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "localhost"
	}
	return "http://" + net.JoinHostPort(host, strconv.Itoa(a.Port))
}

func (a *ApiConfig) GetAPIPath() string {
	return a.GetWebUrl() + normalizePathPrefix(a.ApiPathPrefix)
}

func (a *ApiConfig) GetA2APublicURL() string {
	publicPath := a.A2APublicPath
	if publicPath == "" {
		publicPath = a.A2APath
	}
	result, err := url.JoinPath(a.GetWebUrl(), normalizeRoutePath(publicPath))
	if err != nil {
		return a.GetWebUrl()
	}
	return result
}

func (a *ApiConfig) ResolveAgentCardURL(request *http.Request) string {
	if a.AgentCardURLResolver != nil {
		if resolved := strings.TrimSpace(a.AgentCardURLResolver(request, a)); resolved != "" {
			return resolved
		}
	}
	return a.GetA2APublicURL()
}

func ForwardedAgentCardURL(request *http.Request, config *ApiConfig) string {
	if request == nil || config == nil {
		return ""
	}
	scheme := request.URL.Scheme
	if forwarded := request.Header.Get("X-Forwarded-Proto"); forwarded != "" {
		candidate := strings.TrimSpace(strings.SplitN(forwarded, ",", 2)[0])
		if candidate == "http" || candidate == "https" {
			scheme = candidate
		}
	}
	if scheme == "" {
		scheme = "http"
	}
	host := request.Host
	if host == "" {
		host = request.URL.Host
	}
	if host == "" {
		return ""
	}
	publicPath := config.A2APublicPath
	if publicPath == "" {
		publicPath = config.A2APath
	}
	return scheme + "://" + host + normalizeRoutePath(publicPath)
}

func (a *ApiConfig) ListenAddress() string {
	return net.JoinHostPort(a.Host, strconv.Itoa(a.Port))
}

func normalizePathPrefix(p string) string {
	p = strings.TrimSpace(p)
	if p == "" || p == "/" {
		return ""
	}
	return "/" + strings.Trim(p, "/")
}

func normalizeRoutePath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" || p == "/" {
		return "/"
	}
	return "/" + strings.Trim(p, "/")
}

func Run(ctx context.Context, config *RunConfig, app BasicApp) error {
	if ctx == nil {
		return fmt.Errorf("context is required")
	}
	if config == nil {
		return fmt.Errorf("run config is required")
	}
	if app == nil || app.GetApiConfig() == nil {
		return fmt.Errorf("app with API config is required")
	}
	router := web.BuildBaseRouter()

	if config.SessionService == nil {
		config.SessionService = session.InMemoryService()
	}

	if !config.DisableObservability {
		config.AppendObservability()
		defer func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := observability.Shutdown(shutdownCtx); err != nil {
				log.Errorf("shutting down observability error: %s", err.Error())
				return
			}
			log.Info("observability stopped")
		}()
	}

	log.Infof("Web servers starts on %s", app.GetApiConfig().GetWebUrl())
	err := app.SetupRouters(router, config)
	if err != nil {
		return fmt.Errorf("setup %s routers failed: %w", app.GetServerName(), err)
	}

	srv := http.Server{
		Addr:         app.GetApiConfig().ListenAddress(),
		WriteTimeout: app.GetApiConfig().WriteTimeout,
		ReadTimeout:  app.GetApiConfig().ReadTimeout,
		IdleTimeout:  app.GetApiConfig().IdleTimeout,
		Handler:      router,
	}

	runCtx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	serverErr := make(chan error, 1)
	go func() {
		serverErr <- srv.ListenAndServe()
	}()

	select {
	case err = <-serverErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("%s failed: %w", app.GetServerName(), err)
		}
	case <-runCtx.Done():
		log.Infof("Received shutdown request, gracefully stopping %s...", app.GetServerName())
		shutdownTimeout := app.GetApiConfig().ShutdownTimeout
		if shutdownTimeout <= 0 {
			shutdownTimeout = 30 * time.Second
		}
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if shutdownErr := srv.Shutdown(shutdownCtx); shutdownErr != nil {
			closeErr := srv.Close()
			var listenErr error
			select {
			case listenErr = <-serverErr:
				if errors.Is(listenErr, http.ErrServerClosed) {
					listenErr = nil
				}
			case <-time.After(2 * time.Second):
				listenErr = fmt.Errorf("server did not stop within 2s after forced close")
			}
			return fmt.Errorf("shutdown %s failed: %w", app.GetServerName(), errors.Join(shutdownErr, closeErr, listenErr))
		}
		err = <-serverErr
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("%s failed while shutting down: %w", app.GetServerName(), err)
		}
	}

	log.Infof("%s stopped gracefully", app.GetServerName())
	return nil
}
