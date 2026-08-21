// Package main implements the db-isolation MCP server.
//
// MCP is a JSON-RPC 2.0 protocol carried over stdin/stdout. The adapter
// here is intentionally thin: every tool call is translated into an HTTP
// request to the db-isolation server. The MCP process NEVER talks to MySQL
// directly and NEVER reads secret files.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// JSON-RPC types ---------------------------------------------------------

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string `json:"jsonrpc"`
	ID      any    `json:"id,omitempty"`
	Result  any    `json:"result,omitempty"`
	Error   *rpcError `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

const (
	parseError     = -32700
	invalidRequest = -32600
	methodNotFound = -32601
	internalError  = -32603
)

// MCP protocol types -----------------------------------------------------

type initializeResult struct {
	ProtocolVersion string                 `json:"protocolVersion"`
	ServerInfo      mcpServerInfo         `json:"serverInfo"`
	Capabilities    map[string]any         `json:"capabilities"`
}

type mcpServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type toolsListResult struct {
	Tools []mcpTool `json:"tools"`
}

type mcpTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema mcpInputSchema `json:"inputSchema"`
}

type mcpInputSchema struct {
	Type       string                    `json:"type"`
	Properties map[string]map[string]any  `json:"properties"`
	Required   []string                  `json:"required,omitempty"`
}

type toolsCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

type toolsCallResult struct {
	Content []mcpContent `json:"content"`
	IsError bool         `json:"isError,omitempty"`
}

type mcpContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// main --------------------------------------------------------------------

func main() {
	url := flag.String("url", envOr("DB_ISOLATION_URL", "http://127.0.0.1:8787"), "db-isolation server base URL")
	token := flag.String("token", envOr("DB_ISOLATION_TOKEN", ""), "bearer token")
	flag.Parse()

	cli := &http.Client{Timeout: 30 * time.Second}
	srv := &mcpServer{
		baseURL: strings.TrimRight(*url, "/"),
		token:   *token,
		client:  cli,
	}
	if err := srv.run(os.Stdin, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "mcp: %v\n", err)
		os.Exit(1)
	}
}

func envOr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

// mcpServer is a stdio JSON-RPC server.
type mcpServer struct {
	baseURL string
	token   string
	client  *http.Client
}

func (s *mcpServer) run(in io.Reader, out io.Writer) error {
	// MCP servers should not print to stdout except for valid JSON-RPC
	// frames. We log to stderr so client tooling can ignore us.
	log := func(format string, args ...any) { fmt.Fprintf(os.Stderr, "mcp: "+format+"\n", args...) }

	dec := json.NewDecoder(in)
	enc := json.NewEncoder(out)
	for {
		var req rpcRequest
		if err := dec.Decode(&req); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			log("decode: %v", err)
			return err
		}
		resp := s.handle(req)
		if resp == nil {
			// Notification — no response expected.
			continue
		}
		if err := enc.Encode(resp); err != nil {
			log("encode: %v", err)
			return err
		}
	}
}

func (s *mcpServer) handle(req rpcRequest) *rpcResponse {
	mkErr := func(code int, msg string, data any) *rpcResponse {
		return &rpcResponse{JSONRPC: "2.0", ID: req.ID,
			Error: &rpcError{Code: code, Message: msg, Data: data}}
	}
	mkOK := func(v any) *rpcResponse {
		return &rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: v}
	}

	switch req.Method {
	case "initialize":
		return mkOK(initializeResult{
			ProtocolVersion: "2024-11-05",
			ServerInfo:      mcpServerInfo{Name: "db-isolation", Version: "0.1.0"},
			Capabilities:    map[string]any{"tools": map[string]any{}},
		})
	case "notifications/initialized":
		return nil
	case "tools/list":
		return mkOK(toolsListResult{Tools: s.tools()})
	case "tools/call":
		var p toolsCallParams
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return mkErr(invalidRequest, "invalid params", err.Error())
		}
		result, err := s.callTool(p)
		if err != nil {
			return mkOK(toolsCallResult{
				Content: []mcpContent{{Type: "text", Text: fmt.Sprintf("error: %v", err)}},
				IsError: true,
			})
		}
		return mkOK(result)
	case "ping":
		return mkOK(map[string]any{})
	default:
		return mkErr(methodNotFound, "unknown method: "+req.Method, nil)
	}
}

// tools describes the MCP tool surface.
func (s *mcpServer) tools() []mcpTool {
	stringProp := func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
	return []mcpTool{
		{
			Name: "database_list",
			Description: "List all provisioned database projects. Returns name, database, user, status. " +
				"Never returns passwords.",
			InputSchema: mcpInputSchema{Type: "object", Properties: map[string]map[string]any{}},
		},
		{
			Name: "database_status",
			Description: "Get the status of a single database project by name. Returns name, database, user, " +
				"secret_path, status. Never returns passwords.",
			InputSchema: mcpInputSchema{
				Type:       "object",
				Properties: map[string]map[string]any{"name": stringProp("project name (e.g. opcos)")},
				Required:   []string{"name"},
			},
		},
		{
			Name: "database_create",
			Description: "Create a new database project. The server provisions a dedicated MySQL database and " +
				"user. The response NEVER contains the password — applications must read the on-disk secret " +
				"file (0600) on the server to obtain their credentials.",
			InputSchema: mcpInputSchema{
				Type:       "object",
				Properties: map[string]map[string]any{"name": stringProp("project name (a-z, 0-9, -, _)")},
				Required:   []string{"name"},
			},
		},
		{
			Name: "database_rotate",
			Description: "Rotate the database user password and rewrite the on-disk secret file. The new " +
				"password is never returned.",
			InputSchema: mcpInputSchema{
				Type:       "object",
				Properties: map[string]map[string]any{"name": stringProp("project name")},
				Required:   []string{"name"},
			},
		},
		{
			Name: "database_delete",
			Description: "DESTRUCTIVE: drop the database, user, secret file, and metadata. " +
				"Only call this when the user has explicitly asked to delete a database. " +
				"Requires BOTH `name` and `confirm` to match exactly.",
			InputSchema: mcpInputSchema{
				Type: "object",
				Properties: map[string]map[string]any{
					"name":    stringProp("project name"),
					"confirm": stringProp("must equal the project name exactly"),
				},
				Required: []string{"name", "confirm"},
			},
		},
	}
}

// callTool implements each tool by calling the HTTP API.
func (s *mcpServer) callTool(p toolsCallParams) (toolsCallResult, error) {
	if s.token == "" {
		return toolsCallResult{}, errors.New("missing DB_ISOLATION_TOKEN")
	}

	var args map[string]any
	if len(p.Arguments) > 0 {
		if err := json.Unmarshal(p.Arguments, &args); err != nil {
			return toolsCallResult{}, fmt.Errorf("invalid arguments: %w", err)
		}
	}

	switch p.Name {
	case "database_list":
		body, err := s.do("GET", "/v1/databases", nil)
		if err != nil {
			return toolsCallResult{}, err
		}
		return toolsCallResult{Content: []mcpContent{{Type: "text", Text: body}}}, nil

	case "database_status":
		name, _ := args["name"].(string)
		if name == "" {
			return toolsCallResult{}, errors.New("name is required")
		}
		body, err := s.do("GET", "/v1/databases/"+name, nil)
		if err != nil {
			return toolsCallResult{}, err
		}
		return toolsCallResult{Content: []mcpContent{{Type: "text", Text: body}}}, nil

	case "database_create":
		name, _ := args["name"].(string)
		if name == "" {
			return toolsCallResult{}, errors.New("name is required")
		}
		body, err := s.do("POST", "/v1/databases", map[string]any{"name": name})
		if err != nil {
			return toolsCallResult{}, err
		}
		return toolsCallResult{Content: []mcpContent{{Type: "text", Text: body}}}, nil

	case "database_rotate":
		name, _ := args["name"].(string)
		if name == "" {
			return toolsCallResult{}, errors.New("name is required")
		}
		body, err := s.do("POST", "/v1/databases/"+name+"/rotate", nil)
		if err != nil {
			return toolsCallResult{}, err
		}
		return toolsCallResult{Content: []mcpContent{{Type: "text", Text: body}}}, nil

	case "database_delete":
		name, _ := args["name"].(string)
		confirm, _ := args["confirm"].(string)
		if name == "" || confirm == "" {
			return toolsCallResult{}, errors.New("name and confirm are required")
		}
		body, err := s.do("DELETE", "/v1/databases/"+name, map[string]any{"confirm": confirm})
		if err != nil {
			return toolsCallResult{}, err
		}
		return toolsCallResult{Content: []mcpContent{{Type: "text", Text: body}}}, nil

	default:
		return toolsCallResult{}, errors.New("unknown tool: " + p.Name)
	}
}

// do performs an HTTP call to the db-isolation server. body may be nil.
func (s *mcpServer) do(method, path string, body any) (string, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return "", err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, s.baseURL+path, rdr)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		var env struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(raw, &env)
		if env.Error.Code != "" {
			return "", fmt.Errorf("%s: %s", env.Error.Code, env.Error.Message)
		}
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(raw))
	}
	// Pretty-print for the agent.
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, raw, "", "  "); err == nil {
		return pretty.String(), nil
	}
	return string(raw), nil
}

// silence unused-import warnings under partial builds.
var _ = bufio.NewScanner
var _ sync.Mutex