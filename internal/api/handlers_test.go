package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daifuyang/db-isolation/internal/audit"
	"github.com/daifuyang/db-isolation/internal/auth"
	"github.com/daifuyang/db-isolation/internal/config"
	"github.com/daifuyang/db-isolation/internal/model"
	"github.com/daifuyang/db-isolation/internal/secrets"
	"github.com/daifuyang/db-isolation/internal/store"
)

// stubProvisioner stands in for *provision.Service so we can exercise the
// HTTP layer without MySQL.
type stubProvisioner struct {
	projects map[string]model.DatabaseProject
}

func newTestServer(t *testing.T) (*httptest.Server, string, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "dbi.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	cfg := config.Default()
	cfg.Storage.Path = filepath.Join(dir, "dbi.db")
	cfg.Audit = config.AuditConfig{ToFile: false}
	au, err := audit.New(cfg.Audit, st)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	t.Cleanup(func() { _ = au.Close() })

	sc, err := secrets.New(filepath.Join(dir, "secrets"))
	if err != nil {
		t.Fatalf("secrets: %v", err)
	}

	svc := buildService(st, sc)

	raw, err := model.RandomToken()
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	if _, err := st.CreateToken(context.Background(), "test", auth.HashToken(raw)); err != nil {
		t.Fatalf("create token: %v", err)
	}

	srv := &Server{
		Verifier: auth.NewVerifier(st),
		Service:  svc,
		Audit:    au,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Config:   cfg,
	}
	hs := httptest.NewServer(srv.Routes())
	t.Cleanup(hs.Close)
	return hs, raw, st
}

func TestHealth(t *testing.T) {
	ts, _, _ := newTestServer(t)
	resp, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body["status"] != "ok" {
		t.Fatalf("body = %v", body)
	}
}

func TestUnauthorized(t *testing.T) {
	ts, _, _ := newTestServer(t)
	resp, err := http.Get(ts.URL + "/v1/databases")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestWrongTokenRejected(t *testing.T) {
	ts, _, _ := newTestServer(t)
	req, _ := http.NewRequest("GET", ts.URL+"/v1/databases", nil)
	req.Header.Set("Authorization", "Bearer dbi_wrong")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestRevokedTokenRejected(t *testing.T) {
	ts, raw, st := newTestServer(t)
	// Revoke via store
	tok, err := st.GetTokenByHash(context.Background(), auth.HashToken(raw))
	if err != nil {
		t.Fatalf("get token: %v", err)
	}
	if err := st.RevokeToken(context.Background(), tok.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	req, _ := http.NewRequest("GET", ts.URL+"/v1/databases", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestCreateInvalidName(t *testing.T) {
	ts, raw, _ := newTestServer(t)
	for _, bad := range []string{"", "opcos;DROP", "../../etc", "weird name"} {
		body, _ := json.Marshal(map[string]string{"name": bad})
		req, _ := http.NewRequest("POST", ts.URL+"/v1/databases", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+raw)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("do %q: %v", bad, err)
		}
		if resp.StatusCode != 400 {
			t.Fatalf("name %q: status = %d", bad, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

func TestCreateAndList(t *testing.T) {
	ts, raw, _ := newTestServer(t)

	// create
	body, _ := json.Marshal(map[string]string{"name": "opcos"})
	req, _ := http.NewRequest("POST", ts.URL+"/v1/databases", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+raw)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if resp.StatusCode != 201 {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("create status = %d body=%s", resp.StatusCode, string(raw))
	}
	var pv ProjectView
	_ = json.NewDecoder(resp.Body).Decode(&pv)
	resp.Body.Close()

	// idempotent re-create returns 200
	req2, _ := http.NewRequest("POST", ts.URL+"/v1/databases", bytes.NewReader(body))
	req2.Header.Set("Authorization", "Bearer "+raw)
	req2.Header.Set("Content-Type", "application/json")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("recreate: %v", err)
	}
	if resp2.StatusCode != 200 {
		t.Fatalf("recreate status = %d", resp2.StatusCode)
	}
	resp2.Body.Close()

	// list
	req3, _ := http.NewRequest("GET", ts.URL+"/v1/databases", nil)
	req3.Header.Set("Authorization", "Bearer "+raw)
	resp3, err := http.DefaultClient.Do(req3)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	defer resp3.Body.Close()
	var listResp struct {
		Items []ProjectView `json:"items"`
	}
	_ = json.NewDecoder(resp3.Body).Decode(&listResp)
	if len(listResp.Items) != 1 || listResp.Items[0].Name != "opcos" {
		t.Fatalf("list = %+v", listResp)
	}
}

func TestRotateDoesNotReturnPassword(t *testing.T) {
	ts, raw, _ := newTestServer(t)
	// create
	body, _ := json.Marshal(map[string]string{"name": "opcos"})
	req, _ := http.NewRequest("POST", ts.URL+"/v1/databases", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+raw)
	req.Header.Set("Content-Type", "application/json")
	resp, _ := http.DefaultClient.Do(req)
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Fatalf("create: %d %s", resp.StatusCode, string(respBody))
	}
	if jsonContainsSensitive(string(respBody)) {
		t.Fatalf("create response leaks secret: %s", string(respBody))
	}
	// rotate
	req2, _ := http.NewRequest("POST", ts.URL+"/v1/databases/opcos/rotate", nil)
	req2.Header.Set("Authorization", "Bearer "+raw)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	body2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != 200 {
		t.Fatalf("rotate: %d %s", resp2.StatusCode, string(body2))
	}
	if jsonContainsSensitive(string(body2)) {
		t.Fatalf("rotate response leaks sensitive data: %s", string(body2))
	}
}

// jsonContainsSensitive walks the JSON object looking for keys that look
// like credential fields. We intentionally re-decode into a map so that
// values in unrelated keys (like secret_path) don't trigger the check.
func jsonContainsSensitive(payload string) bool {
	var doc map[string]any
	if err := json.Unmarshal([]byte(payload), &doc); err != nil {
		return false
	}
	return findSensitive(doc, "")
}

func findSensitive(v any, path string) bool {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			if isSensitiveKey(k) {
				return true
			}
			if findSensitive(val, path+"."+k) {
				return true
			}
		}
	case []any:
		for _, item := range x {
			if findSensitive(item, path+"[]") {
				return true
			}
		}
	}
	return false
}

func isSensitiveKey(k string) bool {
	lk := strings.ToLower(k)
	switch lk {
	case "password", "db_password", "database_url", "secret", "dsn":
		return true
	}
	return false
}

func TestDeleteRequiresConfirm(t *testing.T) {
	ts, raw, _ := newTestServer(t)
	// create
	body, _ := json.Marshal(map[string]string{"name": "opcos"})
	req, _ := http.NewRequest("POST", ts.URL+"/v1/databases", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+raw)
	req.Header.Set("Content-Type", "application/json")
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()

	// delete without confirm
	req2, _ := http.NewRequest("DELETE", ts.URL+"/v1/databases/opcos", nil)
	req2.Header.Set("Authorization", "Bearer "+raw)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if resp2.StatusCode != 400 {
		t.Fatalf("expected 400 without confirm, got %d", resp2.StatusCode)
	}
	resp2.Body.Close()

	// delete with wrong confirm
	body3, _ := json.Marshal(map[string]string{"confirm": "nope"})
	req3, _ := http.NewRequest("DELETE", ts.URL+"/v1/databases/opcos", bytes.NewReader(body3))
	req3.Header.Set("Authorization", "Bearer "+raw)
	req3.Header.Set("Content-Type", "application/json")
	resp3, err := http.DefaultClient.Do(req3)
	if err != nil {
		t.Fatalf("delete wrong confirm: %v", err)
	}
	if resp3.StatusCode != 400 {
		t.Fatalf("expected 400 for wrong confirm, got %d", resp3.StatusCode)
	}
	resp3.Body.Close()

	// delete with correct confirm
	req4, _ := http.NewRequest("DELETE", ts.URL+"/v1/databases/opcos",
		bytes.NewReader([]byte(`{"confirm":"opcos"}`)))
	req4.Header.Set("Authorization", "Bearer "+raw)
	req4.Header.Set("Content-Type", "application/json")
	resp4, err := http.DefaultClient.Do(req4)
	if err != nil {
		t.Fatalf("delete ok: %v", err)
	}
	if resp4.StatusCode != 204 {
		raw, _ := io.ReadAll(resp4.Body)
		t.Fatalf("expected 204, got %d %s", resp4.StatusCode, string(raw))
	}
	resp4.Body.Close()
}