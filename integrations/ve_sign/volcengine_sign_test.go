package ve_sign

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestValidateOK(t *testing.T) {
	v := VeRequest{
		AK:      "ak",
		SK:      "sk",
		Method:  "POST",
		Scheme:  HttpsSchema,
		Host:    "open.volcengineapi.com",
		Path:    "/v1/test",
		Service: "open",
		Region:  "cn-beijing",
		Body:    []byte("{}"),
	}
	if err := v.validate(); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestValidateEmptyAK(t *testing.T) {
	v := VeRequest{SK: "sk", Method: "GET", Host: "h.com", Path: "/p", Service: "s", Region: "r"}
	if err := v.validate(); err == nil {
		t.Fatalf("expected error for empty AK")
	}
}

func TestValidateInvalidMethod(t *testing.T) {
	v := VeRequest{AK: "ak", SK: "sk", Method: "FOO", Host: "h.com", Path: "/p", Service: "s", Region: "r"}
	if err := v.validate(); err == nil {
		t.Fatalf("expected error for invalid method")
	}
}

func TestValidateInvalidHost(t *testing.T) {
	v := VeRequest{AK: "ak", SK: "sk", Method: "GET", Host: "", Path: "/p", Service: "s", Region: "r"}
	if err := v.validate(); err == nil {
		t.Fatalf("expected error for empty host")
	}
	v.Host = "h.com/extra"
	if err := v.validate(); err == nil {
		t.Fatalf("expected error for host containing path")
	}
}

func TestValidateInvalidPath(t *testing.T) {
	v := VeRequest{AK: "ak", SK: "sk", Method: "GET", Host: "h.com", Path: "p", Service: "s", Region: "r"}
	if err := v.validate(); err == nil {
		t.Fatalf("expected error for path not starting with /")
	}
}

func TestValidateEmptyBodyForPOST(t *testing.T) {
	v := VeRequest{AK: "ak", SK: "sk", Method: "POST", Host: "h.com", Path: "/p", Service: "s", Region: "r"}
	if err := v.validate(); err == nil {
		t.Fatalf("expected error for empty body on POST")
	}
}

func TestDoRequestWithContextBoundsResponseBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", maxVeResponseBodyBytes+1)))
	}))
	defer server.Close()

	request := VeRequest{
		AK: "ak", SK: "sk", Method: http.MethodGet,
		Scheme: HttpSchema, Host: strings.TrimPrefix(server.URL, "http://"), Path: "/",
		Service: "test", Region: "test", Action: "Test", Version: "1",
	}
	if _, err := request.DoRequestWithContext(context.Background(), server.Client()); err == nil || !strings.Contains(err.Error(), "response body exceeds") {
		t.Fatalf("DoRequestWithContext() error = %v, want bounded response error", err)
	}
}
