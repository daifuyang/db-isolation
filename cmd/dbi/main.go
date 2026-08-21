// Package main implements the `dbi` CLI client. The CLI is a thin wrapper
// around the HTTP API: it never touches MySQL, never reads secret files,
// and never receives database passwords.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Global flags shared by subcommands.
type globalFlags struct {
	URL   string
	Token string
	JSON  bool
}

func main() {
	os.Exit(dispatchMain())
}

// dispatchMain is the testable entry point. It returns the desired exit
// code instead of calling os.Exit so unit tests can observe it.
//
// Argument layout:
//
//	dbi [global flags] <subcommand> [subcommand flags]
//
// Global flags must precede the subcommand; subcommand-specific flags
// (e.g. --confirm, --json) come after.
func dispatchMain() int {
	args := os.Args[1:]
	if len(args) == 0 {
		usage()
		return 2
	}
	g := &globalFlags{}
	fs := flag.NewFlagSet("dbi", flag.ContinueOnError)
	fs.StringVar(&g.URL, "url", envDefault("DB_ISOLATION_URL", "http://127.0.0.1:8787"), "base URL of db-isolation server")
	fs.StringVar(&g.Token, "token", envDefault("DB_ISOLATION_TOKEN", ""), "bearer token (DB_ISOLATION_TOKEN)")
	fs.BoolVar(&g.JSON, "json", false, "output machine-readable JSON")

	// Find the first non-flag argument — that is the subcommand. We must
	// also skip the value that follows a value-taking flag (--url,
	// --token) so the URL/token value is not mistaken for the subcommand.
	takesValue := map[string]bool{"--url": true, "--token": true}
	cmdIdx := -1
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--help" || a == "-h" {
			usage()
			return 0
		}
		if strings.HasPrefix(a, "-") {
			if takesValue[a] {
				i++ // skip the value (range copy shadows, so we use index form)
			}
			continue
		}
		cmdIdx = i
		break
	}
	if cmdIdx < 0 {
		usage()
		return 2
	}
	cmd := args[cmdIdx]
	if err := fs.Parse(args[:cmdIdx]); err != nil {
		return 2
	}
	rest := args[cmdIdx+1:]
	switch cmd {
	case "list":
		return cmdList(g, fs, rest)
	case "status":
		return cmdStatus(g, fs, rest)
	case "create":
		return cmdCreate(g, fs, rest)
	case "rotate":
		return cmdRotate(g, fs, rest)
	case "delete":
		return cmdDelete(g, fs, rest)
	case "help":
		usage()
		return 0
	}
	usage()
	return 2
}

func usage() {
	fmt.Fprintln(os.Stderr, `dbi — db-isolation CLI

Usage:
  dbi list
  dbi status <name>
  dbi create <name>
  dbi rotate <name>
  dbi delete <name> --confirm <name>

Flags (before subcommand):
  --url URL         Server base URL (default http://127.0.0.1:8787)
                    Env: DB_ISOLATION_URL
  --token TOKEN     Bearer token (default: $DB_ISOLATION_TOKEN)
  --json            Output JSON

Examples:
  export DB_ISOLATION_TOKEN=dbi_xxx
  dbi create opcos
  dbi delete opcos --confirm opcos`)
}

func envDefault(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

func (g *globalFlags) requireToken() string {
	if g.Token == "" {
		fmt.Fprintln(os.Stderr, "error: missing bearer token. Pass --token or set DB_ISOLATION_TOKEN.")
		os.Exit(2)
	}
	return g.Token
}

// APIError is the wire shape of an error envelope.
type APIError struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// apiCall performs an HTTP request and returns the decoded body. On error
// it prints a friendly message and exits via exitf in production. For
// tests, the testable variant is used; see apiCallT.
func apiCall(g *globalFlags, method, path string, body any, out any) {
	err := apiCallT(g, method, path, body, out)
	if err != nil {
		exitf("%v", err)
	}
}

// apiCallT is the testable sibling of apiCall: it returns an error instead
// of calling exitf.
func apiCallT(g *globalFlags, method, path string, body any, out any) error {
	var bodyReader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return exitErrf("encode body: %v", err)
		}
		bodyReader = bytes.NewReader(buf)
	}
	req, err := http.NewRequest(method, strings.TrimRight(g.URL, "/")+path, bodyReader)
	if err != nil {
		return exitErrf("build request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+g.requireToken())
	req.Header.Set("Accept", "application/json")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return exitErrf("request failed: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		var env APIError
		_ = json.Unmarshal(raw, &env)
		if env.Error.Code != "" {
			return exitErrf("server returned %s: %s", resp.Status, env.Error.Message)
		}
		return exitErrf("server returned %s: %s", resp.Status, string(raw))
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return exitErrf("decode response: %v", err)
		}
	}
	return nil
}

type projectView struct {
	Name       string `json:"name"`
	Database   string `json:"database"`
	User       string `json:"user"`
	SecretPath string `json:"secret_path"`
	Status     string `json:"status"`
	LastError  string `json:"last_error,omitempty"`
}

type listView struct {
	Items []projectView `json:"items"`
}

func cmdList(g *globalFlags, fs *flag.FlagSet, args []string) int {
	// list takes no subcommand flags. We expect args to be empty.
	if len(args) > 0 {
		fmt.Fprintln(os.Stderr, "usage: dbi list")
		return 2
	}
	var resp listView
	if err := apiCallT(g, "GET", "/v1/databases", nil, &resp); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if g.JSON {
		printJSON(resp)
		return 0
	}
	if len(resp.Items) == 0 {
		fmt.Println("(no databases)")
		return 0
	}
	fmt.Printf("%-20s %-30s %-25s %-10s\n", "NAME", "DATABASE", "USER", "STATUS")
	for _, p := range resp.Items {
		fmt.Printf("%-20s %-30s %-25s %-10s\n",
			p.Name, p.Database, p.User, p.Status)
	}
	return 0
}

func cmdStatus(g *globalFlags, _ *flag.FlagSet, args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: dbi status <name>")
		return 2
	}
	name := args[0]
	var resp projectView
	if err := apiCallT(g, "GET", "/v1/databases/"+name, nil, &resp); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if g.JSON {
		printJSON(resp)
		return 0
	}
	fmt.Printf("Project:    %s\n", resp.Name)
	fmt.Printf("Database:   %s\n", resp.Database)
	fmt.Printf("User:       %s\n", resp.User)
	fmt.Printf("Secret:     %s\n", resp.SecretPath)
	fmt.Printf("Status:     %s\n", resp.Status)
	if resp.LastError != "" {
		fmt.Printf("LastError:  %s\n", resp.LastError)
	}
	return 0
}

func cmdCreate(g *globalFlags, _ *flag.FlagSet, args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: dbi create <name>")
		return 2
	}
	name := args[0]
	body := map[string]string{"name": name}
	var resp projectView
	if err := apiCallT(g, "POST", "/v1/databases", body, &resp); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if g.JSON {
		printJSON(resp)
		return 0
	}
	fmt.Printf("✓ Project: %s\n", resp.Name)
	fmt.Printf("✓ Database: %s\n", resp.Database)
	fmt.Printf("✓ User: %s\n", resp.User)
	fmt.Printf("✓ Secret: %s\n", resp.SecretPath)
	fmt.Printf("✓ Status: %s\n", resp.Status)
	return 0
}

func cmdRotate(g *globalFlags, _ *flag.FlagSet, args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: dbi rotate <name>")
		return 2
	}
	name := args[0]
	var resp projectView
	if err := apiCallT(g, "POST", "/v1/databases/"+name+"/rotate", nil, &resp); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if g.JSON {
		printJSON(resp)
		return 0
	}
	fmt.Printf("✓ Rotated credentials for %s\n", resp.Name)
	fmt.Printf("✓ Status: %s\n", resp.Status)
	return 0
}

func cmdDelete(g *globalFlags, _ *flag.FlagSet, args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: dbi delete <name> --confirm <name>")
		return 2
	}
	name := args[0]
	// Parse --confirm from args.
	confirm := ""
	for i := 1; i < len(args); i++ {
		if args[i] == "--confirm" && i+1 < len(args) {
			confirm = args[i+1]
			break
		}
		if strings.HasPrefix(args[i], "--confirm=") {
			confirm = strings.TrimPrefix(args[i], "--confirm=")
			break
		}
	}
	if confirm == "" {
		fmt.Fprintln(os.Stderr, "refusing to delete without --confirm <name>")
		return 2
	}
	body := map[string]string{"confirm": confirm}
	if err := apiCallT(g, "DELETE", "/v1/databases/"+name, body, nil); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Printf("✓ Deleted %s\n", name)
	return 0
}

func printJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func exitf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	os.Exit(1)
}

// exitErrf is the testable sibling of exitf: it returns an error instead of
// calling os.Exit. Used by apiCall so unit tests can observe failures.
func exitErrf(format string, args ...any) error {
	return fmt.Errorf("error: "+format, args...)
}