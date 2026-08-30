package agentkit_server_app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	a2acore "github.com/a2aproject/a2a-go/a2a"
	"github.com/a2aproject/a2a-go/a2asrv"
	"github.com/gorilla/mux"
	"github.com/volcengine/veadk-go/apps"
	"google.golang.org/adk/agent"
	"google.golang.org/adk/session"
)

func TestSetupRoutersSupportsProductionConfiguration(t *testing.T) {
	rootAgent, err := agent.New(agent.Config{
		Name:        "skill_agent",
		Description: "executes local skills",
	})
	if err != nil {
		t.Fatalf("agent.New() error = %v", err)
	}

	apiConfig := apps.DefaultApiConfig().
		SetHost("127.0.0.1").
		SetPort(8024).
		SetPublicURL("https://example.test/sandbox").
		SetA2APath("/internal-a2a").
		SetA2APublicPath("/a2a").
		SetAgentCardURLResolver(apps.ForwardedAgentCardURL).
		SetSimpleAPIEnabled(false).
		SetWebUIEnabled(false)

	app := NewAgentkitServerApp(
		apiConfig,
		WithMiddleware(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-Test-Middleware", "enabled")
				next.ServeHTTP(w, r)
			})
		}),
		WithRouteSetup(func(router *mux.Router, _ *apps.RunConfig) error {
			router.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			}).Methods(http.MethodGet)
			return nil
		}),
	)

	router := mux.NewRouter()
	err = app.SetupRouters(router, &apps.RunConfig{
		AgentLoader:    agent.NewSingleLoader(rootAgent),
		SessionService: session.InMemoryService(),
	})
	if err != nil {
		t.Fatalf("SetupRouters() error = %v", err)
	}

	healthRequest := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	healthResponse := httptest.NewRecorder()
	router.ServeHTTP(healthResponse, healthRequest)
	if got, want := healthResponse.Code, http.StatusNoContent; got != want {
		t.Fatalf("health status = %d, want %d", got, want)
	}
	if got, want := healthResponse.Header().Get("X-Test-Middleware"), "enabled"; got != want {
		t.Fatalf("middleware header = %q, want %q", got, want)
	}

	cardRequest := httptest.NewRequest(http.MethodGet, a2asrv.WellKnownAgentCardPath, nil)
	cardRequest.Host = "public.example.test"
	cardRequest.Header.Set("X-Forwarded-Proto", "https")
	cardResponse := httptest.NewRecorder()
	router.ServeHTTP(cardResponse, cardRequest)
	if got, want := cardResponse.Code, http.StatusOK; got != want {
		t.Fatalf("agent card status = %d, want %d: %s", got, want, cardResponse.Body.String())
	}
	var card a2acore.AgentCard
	if err := json.NewDecoder(cardResponse.Body).Decode(&card); err != nil {
		t.Fatalf("decode agent card: %v", err)
	}
	if got, want := card.URL, "https://public.example.test/a2a"; got != want {
		t.Fatalf("agent card URL = %q, want %q", got, want)
	}

	legacyCardRequest := httptest.NewRequest(http.MethodGet, "/.well-known/agent.json", nil)
	legacyCardResponse := httptest.NewRecorder()
	router.ServeHTTP(legacyCardResponse, legacyCardRequest)
	if got, want := legacyCardResponse.Code, http.StatusOK; got != want {
		t.Fatalf("legacy agent card status = %d, want %d", got, want)
	}
}

func TestSetupRoutersRejectsMissingAgent(t *testing.T) {
	app := NewAgentkitServerApp(nil)
	if err := app.SetupRouters(mux.NewRouter(), &apps.RunConfig{}); err == nil {
		t.Fatal("SetupRouters() error = nil, want missing agent error")
	}
}
