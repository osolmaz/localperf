package runner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestProbeEngineIdentity(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/version":
			_, _ = w.Write([]byte(`{"version":"0.11.0"}`))
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"served/model-a"},{"id":"served/model-b"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	host, port := testServerHostPort(t, server)
	profile := Profile{Name: "p", Host: host, Port: port}
	client := &http.Client{Timeout: 3 * time.Second}

	identity, ok := probeEngineIdentity(context.Background(), client, profile)
	if !ok || identity.Version != "0.11.0" {
		t.Fatalf("identity = %+v ok=%t, want version 0.11.0", identity, ok)
	}
	var models struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(identity.Models, &models); err != nil || len(models.Data) != 2 {
		t.Fatalf("models = %s err=%v, want two served models", identity.Models, err)
	}
}

func TestProbeEngineIdentityAbsentEndpoints(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer server.Close()
	host, port := testServerHostPort(t, server)
	profile := Profile{Name: "p", Host: host, Port: port}
	client := &http.Client{Timeout: 3 * time.Second}
	if _, ok := probeEngineIdentity(context.Background(), client, profile); ok {
		t.Fatal("identity reported ok for a server with no identity endpoints")
	}
}
