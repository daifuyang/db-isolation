// db-isolation server binary.
//
// Subcommands:
//
//	server          Run the HTTP API.
//	token create    Create a new admin bearer token.
//	token list      List tokens.
//	token revoke    Revoke a token by id.
//	audit list      List recent audit log entries.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/daifuyang/db-isolation/internal/api"
	"github.com/daifuyang/db-isolation/internal/audit"
	"github.com/daifuyang/db-isolation/internal/auth"
	"github.com/daifuyang/db-isolation/internal/config"
	"github.com/daifuyang/db-isolation/internal/model"
	"github.com/daifuyang/db-isolation/internal/mysqlx"
	"github.com/daifuyang/db-isolation/internal/provision"
	"github.com/daifuyang/db-isolation/internal/secrets"
	"github.com/daifuyang/db-isolation/internal/store"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "server":
		os.Exit(runServer(os.Args[2:]))
	case "token":
		os.Exit(runToken(os.Args[2:]))
	case "audit":
		os.Exit(runAudit(os.Args[2:]))
	case "-h", "--help", "help":
		usage()
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `db-isolation server binary

Usage:
  db-isolation server [--config PATH] [--addr HOST:PORT]
  db-isolation token create --name NAME [--config PATH]
  db-isolation token list [--config PATH]
  db-isolation token revoke --id ID [--config PATH]
  db-isolation audit list [--limit N] [--config PATH]`)
}

func loadConfigAndStore(args []string, flagName string) (config.Config, *store.Store, error) {
	// Find --config anywhere in args (including after the subcommand),
	// since the helper is called from subcommands like
	// `db-isolation token create --name X --config /path`.
	configPath := "/etc/db-isolation/config.yaml"
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--config" && i+1 < len(args) {
			configPath = args[i+1]
			break
		}
		if strings.HasPrefix(a, "--config=") {
			configPath = strings.TrimPrefix(a, "--config=")
			break
		}
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return cfg, nil, err
	}
	st, err := store.Open(cfg.Storage.Path)
	if err != nil {
		return cfg, nil, err
	}
	return cfg, st, nil
}

// runServer starts the HTTP server.
func runServer(args []string) int {
	fs := flag.NewFlagSet("server", flag.ContinueOnError)
	configPath := fs.String("config", "/etc/db-isolation/config.yaml", "path to YAML config")
	addr := fs.String("addr", "", "override listen address")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return 1
	}
	if *addr != "" {
		cfg.Server.Addr = *addr
	}

	logger := newLogger(cfg.Logging.Level)

	st, err := store.Open(cfg.Storage.Path)
	if err != nil {
		logger.Error("open store", "error", err.Error())
		return 1
	}
	defer st.Close()

	auditLog, err := audit.New(cfg.Audit, st)
	if err != nil {
		logger.Error("init audit", "error", err.Error())
		return 1
	}
	defer auditLog.Close()

	dsn, err := resolveAdminDSN(cfg.MySQL)
	if err != nil {
		logger.Error("resolve admin DSN", "error", err.Error())
		return 1
	}
	mysqlConn, err := mysqlx.New(dsn)
	if err != nil {
		logger.Error("connect MySQL", "error", redactDSN(err.Error()))
		return 1
	}
	defer mysqlConn.Close()

	secretWriter, err := secrets.New(cfg.Secrets.Dir)
	if err != nil {
		logger.Error("init secrets", "error", err.Error())
		return 1
	}

	svc := provision.New(st, mysqlConn, secretWriter, auditLog, logger, cfg.MySQL)
	verifier := auth.NewVerifier(st)
	srv := &api.Server{
		Verifier: verifier, Service: svc, Audit: auditLog,
		Logger: logger, Config: cfg,
	}

	httpServer := &http.Server{
		Addr:              cfg.Server.Addr,
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// Graceful shutdown
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", cfg.Server.Addr)
		errCh <- httpServer.ListenAndServe()
	}()
	select {
	case <-ctx.Done():
		logger.Info("shutting down")
		shutCtx, shutCancel := context.WithTimeout(context.Background(),
			time.Duration(cfg.Server.ShutdownTimeout)*time.Second)
		defer shutCancel()
		if err := httpServer.Shutdown(shutCtx); err != nil {
			logger.Error("shutdown", "error", err.Error())
			return 1
		}
		return 0
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("listen", "error", err.Error())
			return 1
		}
		return 0
	}
}

func runToken(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: db-isolation token <create|list|revoke> ...")
		return 2
	}
	cfg, st, err := loadConfigAndStore(args, "config")
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return 1
	}
	defer st.Close()
	logger := newLogger(cfg.Logging.Level)

	// Strip --config out of args before passing to subcommand parsers so
	// they don't reject the unknown flag.
	clean := stripConfig(args)

	switch clean[0] {
	case "create":
		fs := flag.NewFlagSet("token create", flag.ContinueOnError)
		name := fs.String("name", "", "friendly token name (required)")
		if err := fs.Parse(clean[1:]); err != nil {
			return 2
		}
		if strings.TrimSpace(*name) == "" {
			fmt.Fprintln(os.Stderr, "--name is required")
			return 2
		}
		raw, err := model.RandomToken()
		if err != nil {
			fmt.Fprintln(os.Stderr, "generate token:", err)
			return 1
		}
		if _, err := st.CreateToken(context.Background(), *name, auth.HashToken(raw)); err != nil {
			fmt.Fprintln(os.Stderr, "store token:", err)
			return 1
		}
		logger.Info("token created", "name", *name)
		fmt.Println("Token created. This token will only be shown once.")
		fmt.Println()
		fmt.Println(raw)
		fmt.Println()
		fmt.Println("Save it now. Store as DB_ISOLATION_TOKEN.")
		return 0

	case "list":
		tokens, err := st.ListTokens(context.Background())
		if err != nil {
			fmt.Fprintln(os.Stderr, "list tokens:", err)
			return 1
		}
		fmt.Printf("%-4s %-20s %-22s %-22s %-22s\n",
			"ID", "NAME", "CREATED", "LAST USED", "REVOKED")
		for _, t := range tokens {
			fmt.Printf("%-4d %-20s %-22s %-22s %-22s\n",
				t.ID, t.Name,
				t.CreatedAt.Format(time.RFC3339),
				formatOptTime(t.LastUsedAt),
				formatOptTime(t.RevokedAt),
			)
		}
		return 0

	case "revoke":
		fs := flag.NewFlagSet("token revoke", flag.ContinueOnError)
		id := fs.Int64("id", 0, "token id to revoke")
		if err := fs.Parse(clean[1:]); err != nil {
			return 2
		}
		if *id <= 0 {
			fmt.Fprintln(os.Stderr, "--id is required")
			return 2
		}
		if err := st.RevokeToken(context.Background(), *id); err != nil {
			fmt.Fprintln(os.Stderr, "revoke:", err)
			return 1
		}
		logger.Info("token revoked", "id", *id)
		return 0
	}
	fmt.Fprintln(os.Stderr, "unknown subcommand:", args[0])
	return 2
}

func runAudit(args []string) int {
	if len(args) < 1 || args[0] != "list" {
		fmt.Fprintln(os.Stderr, "usage: db-isolation audit list [--limit N]")
		return 2
	}
	fs := flag.NewFlagSet("audit list", flag.ContinueOnError)
	limit := fs.Int("limit", 50, "max entries")
	clean := stripConfig(args)
	if err := fs.Parse(clean[1:]); err != nil {
		return 2
	}
	_, st, err := loadConfigAndStore(args, "config")
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return 1
	}
	defer st.Close()
	entries, err := st.ListAudit(context.Background(), *limit)
	if err != nil {
		fmt.Fprintln(os.Stderr, "list audit:", err)
		return 1
	}
	for _, e := range entries {
		fmt.Printf("%s %-22s resource=%-8s name=%-16s ok=%-5v remote=%s msg=%q\n",
			e.CreatedAt.Format(time.RFC3339), e.Action, e.Resource, e.ResourceName,
			e.Success, e.RemoteIP, e.Message)
	}
	return 0
}

// stripConfig removes --config PATH (or --config=PATH) tokens from args
// so the leftover slice is safe to feed to a subcommand FlagSet that does
// not know about --config.
func stripConfig(args []string) []string {
	out := make([]string, 0, len(args))
	skip := false
	for _, a := range args {
		if skip {
			skip = false
			continue
		}
		if a == "--config" {
			skip = true
			continue
		}
		if strings.HasPrefix(a, "--config=") {
			continue
		}
		out = append(out, a)
	}
	return out
}

// resolveAdminDSN resolves the admin DSN from MYSQL_ADMIN_DSN or the
// server-side my.cnf. We never read MySQL credentials from YAML or flags.
func resolveAdminDSN(cfg config.MySQLConfig) (string, error) {
	if cfg.AdminDSN != "" {
		return cfg.AdminDSN, nil
	}
	if cfg.AdminConfigPath == "" {
		return "", errors.New("no admin DSN: set MYSQL_ADMIN_DSN or my_admin_config_path")
	}
	mc, err := config.LoadMycnf(cfg.AdminConfigPath)
	if err != nil {
		return "", err
	}
	if mc.User == "" {
		return "", fmt.Errorf("no user in %s", cfg.AdminConfigPath)
	}
	return mc.DSN(), nil
}

// newLogger returns a slog.Logger writing to stderr with text format. The
// default logger must NEVER include secrets.
func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}

// redactDSN replaces the password component of a DSN with *** for safe
// logging. We never log the raw DSN.
func redactDSN(msg string) string {
	// crude: anything between : and @ before the host
	for i := 0; i < len(msg); i++ {
		if msg[i] == ':' && i+1 < len(msg) {
			// find next '@'
			j := strings.IndexByte(msg[i+1:], '@')
			if j >= 0 {
				return msg[:i+1] + "***" + msg[i+1+j:]
			}
		}
	}
	return msg
}

func formatOptTime(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return t.Format(time.RFC3339)
}

// silence unused-import warnings under partial builds.
var _ = strconv.Itoa