package apps

import (
	"context"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"
)

func TestRunRejectsInvalidInputs(t *testing.T) {
	if err := Run(nil, &RunConfig{}, nil); err == nil {
		t.Fatal("Run(nil context) error = nil")
	}
	if err := Run(context.Background(), nil, nil); err == nil {
		t.Fatal("Run(nil config) error = nil")
	}
	if err := Run(context.Background(), &RunConfig{}, nil); err == nil {
		t.Fatal("Run(nil app) error = nil")
	}
}

func TestAPIConfigAddresses(t *testing.T) {
	config := DefaultApiConfig().
		SetHost("127.0.0.1").
		SetPort(8024).
		SetApiPathPrefix("/api/").
		SetA2APath("internal-a2a").
		SetA2APublicPath("a2a")

	if got, want := config.ListenAddress(), "127.0.0.1:8024"; got != want {
		t.Fatalf("ListenAddress() = %q, want %q", got, want)
	}
	if got, want := config.GetWebUrl(), "http://127.0.0.1:8024"; got != want {
		t.Fatalf("GetWebUrl() = %q, want %q", got, want)
	}
	if got, want := config.GetAPIPath(), "http://127.0.0.1:8024/api"; got != want {
		t.Fatalf("GetAPIPath() = %q, want %q", got, want)
	}
	if got, want := config.A2APath, "/internal-a2a"; got != want {
		t.Fatalf("A2APath = %q, want %q", got, want)
	}
	if got, want := config.GetA2APublicURL(), "http://127.0.0.1:8024/a2a"; got != want {
		t.Fatalf("GetA2APublicURL() = %q, want %q", got, want)
	}
}

func TestForwardedAgentCardURL(t *testing.T) {
	config := DefaultApiConfig().SetA2APublicPath("/a2a")
	request, err := http.NewRequest(http.MethodGet, "http://127.0.0.1/.well-known/agent-card.json", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Host = "sandbox.example.test"
	request.Header.Set("X-Forwarded-Proto", "https")
	if got, want := ForwardedAgentCardURL(request, config), "https://sandbox.example.test/a2a"; got != want {
		t.Fatalf("ForwardedAgentCardURL() = %q, want %q", got, want)
	}
}

func TestAPIConfigUsesPublicURLForAgentEndpoints(t *testing.T) {
	config := DefaultApiConfig().
		SetHost("127.0.0.1").
		SetPort(8024).
		SetPublicURL("https://example.test/tool/agent/").
		SetApiPathPrefix("api")

	if got, want := config.GetWebUrl(), "https://example.test/tool/agent"; got != want {
		t.Fatalf("GetWebUrl() = %q, want %q", got, want)
	}
	if got, want := config.GetAPIPath(), "https://example.test/tool/agent/api"; got != want {
		t.Fatalf("GetAPIPath() = %q, want %q", got, want)
	}
}

type shutdownTestApp struct {
	config  *ApiConfig
	started chan struct{}
	stopped chan struct{}
}

func (a *shutdownTestApp) Run(ctx context.Context, config *RunConfig) error {
	return Run(ctx, config, a)
}

func (a *shutdownTestApp) SetupRouters(router *mux.Router, _ *RunConfig) error {
	router.HandleFunc("/block", func(_ http.ResponseWriter, request *http.Request) {
		close(a.started)
		<-request.Context().Done()
		close(a.stopped)
	})
	return nil
}

func (a *shutdownTestApp) GetApiConfig() *ApiConfig { return a.config }
func (a *shutdownTestApp) GetServerName() string    { return "shutdown-test" }

func TestRunForcesCloseAfterGracefulShutdownTimeout(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}

	app := &shutdownTestApp{
		config:  DefaultApiConfig().SetHost("127.0.0.1").SetPort(port),
		started: make(chan struct{}),
		stopped: make(chan struct{}),
	}
	app.config.ShutdownTimeout = 20 * time.Millisecond
	runCtx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() {
		runErr <- app.Run(runCtx, &RunConfig{DisableObservability: true})
	}()

	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	deadline := time.Now().Add(2 * time.Second)
	for {
		connection, dialErr := net.DialTimeout("tcp", address, 20*time.Millisecond)
		if dialErr == nil {
			_ = connection.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("server did not listen: %v", dialErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
	go func() {
		client := &http.Client{Timeout: 2 * time.Second}
		_, _ = client.Get("http://" + address + "/block")
	}()
	select {
	case <-app.started:
	case <-time.After(2 * time.Second):
		t.Fatal("blocking request did not start")
	}

	cancel()
	select {
	case err := <-runErr:
		if err == nil || !strings.Contains(err.Error(), "shutdown shutdown-test failed") {
			t.Fatalf("Run() error = %v, want shutdown timeout", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run() did not return after forced close")
	}
	select {
	case <-app.stopped:
	case <-time.After(time.Second):
		t.Fatal("forced close did not cancel active handler")
	}
}
