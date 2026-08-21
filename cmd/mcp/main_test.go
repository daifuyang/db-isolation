package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeHTTP stands in for the db-isolation server. We assert that the MCP
// adapter forwards calls to the server and that the responses it relays
// contain no secret material.
type fakeHTTP struct {
	gotAuth string
	calls   []string
}

func (f *fakeHTTP) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.gotAuth = r.Header.Get("Authorization")
		f.calls = append(f.calls, r.Method+" "+r.URL.Path)
		switch {
		case r.URL.Path == "/v1/databases" && r.Method == "GET":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []map[string]string{{
					"name": "opcos", "database": "opcos_db",
					"user": "opcos_user", "status": "ready",
				}},
			})
		case r.URL.Path == "/v1/databases" && r.Method == "POST":
			_ = json.NewEncoder(w).Encode(map[string]string{
				"name": "opcos", "database": "opcos_db",
				"user": "opcos_user", "status": "ready",
				"secret_path": "/etc/db-isolation/apps/opcos.env",
			})
		case strings.HasPrefix(r.URL.Path, "/v1/databases/") && r.Method == "DELETE":
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusOK)
		}
	})
}

func TestMCPInitialize(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer api.Close()
	srv := &mcpServer{baseURL: api.URL, token: "dbi_test", client: api.Client()}

	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`)
	out := &strings.Builder{}
	if err := srv.run(in, out); err != nil {
		t.Fatalf("run: %v", err)
	}
	var resp rpcResponse
	if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("error: %+v", resp.Error)
	}
	if resp.Result == nil {
		t.Fatalf("nil result")
	}
}

func TestMCPToolsList(t *testing.T) {
	srv := &mcpServer{}
	in := strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	out := &strings.Builder{}
	if err := srv.run(in, out); err != nil {
		t.Fatalf("run: %v", err)
	}
	var resp rpcResponse
	_ = json.Unmarshal([]byte(strings.TrimSpace(out.String())), &resp)
	res, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatalf("bad result type")
	}
	tools, _ := res["tools"].([]any)
	if len(tools) != 5 {
		t.Fatalf("expected 5 tools, got %d", len(tools))
	}
	for _, ti := range tools {
		tm, _ := ti.(map[string]any)
		name, _ := tm["name"].(string)
		if name == "execute_sql" || name == "execute_shell" || name == "mysql_root" {
			t.Fatalf("forbidden tool exposed: %s", name)
		}
	}
}

func TestMCPDatabaseCreate(t *testing.T) {
	fh := &fakeHTTP{}
	api := httptest.NewServer(fh.handler(t))
	defer api.Close()

	srv := &mcpServer{baseURL: api.URL, token: "dbi_test", client: api.Client()}
	in := strings.NewReader(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"database_create","arguments":{"name":"opcos"}}}`)
	out := &strings.Builder{}
	if err := srv.run(in, out); err != nil {
		t.Fatalf("run: %v", err)
	}
	if fh.gotAuth != "Bearer dbi_test" {
		t.Errorf("auth = %q", fh.gotAuth)
	}
	if len(fh.calls) != 1 || fh.calls[0] != "POST /v1/databases" {
		t.Errorf("calls = %v", fh.calls)
	}
	var resp rpcResponse
	_ = json.Unmarshal([]byte(strings.TrimSpace(out.String())), &resp)
	res, _ := resp.Result.(map[string]any)
	content, _ := res["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("no content")
	}
	text, _ := content[0].(map[string]any)["text"].(string)
	if strings.Contains(strings.ToLower(text), "password") {
		t.Fatalf("tool output leaks password: %s", text)
	}
}

func TestMCPDatabaseDeleteRequiresConfirm(t *testing.T) {
	srv := &mcpServer{baseURL: "http://127.0.0.1:1", token: "dbi_test", client: http.DefaultClient}
	// Missing confirm should error.
	in := strings.NewReader(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"database_delete","arguments":{"name":"opcos"}}}`)
	out := &strings.Builder{}
	_ = srv.run(in, out)
	var resp rpcResponse
	_ = json.Unmarshal([]byte(strings.TrimSpace(out.String())), &resp)
	res, _ := resp.Result.(map[string]any)
	if isErr, _ := res["isError"].(bool); !isErr {
		t.Fatalf("expected isError=true, got %+v", resp)
	}
}

func TestMCPUnknownMethod(t *testing.T) {
	srv := &mcpServer{}
	in := strings.NewReader(`{"jsonrpc":"2.0","id":5,"method":"foo/bar"}`)
	out := &strings.Builder{}
	_ = srv.run(in, out)
	var resp rpcResponse
	_ = json.Unmarshal([]byte(strings.TrimSpace(out.String())), &resp)
	if resp.Error == nil || resp.Error.Code != methodNotFound {
		t.Fatalf("expected method-not-found, got %+v", resp)
	}
}