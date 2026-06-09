package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"postgresql/internal/tunnel"
	"strconv"
	"strings"
	"syscall"
)

const defaultDBAPI = "https://postgresql.exe.xyz:8000"

func main() {
	if err := run(os.Args[1:]); err != nil {
		log.Fatal(err)
	}
}

func run(args []string) error {
	if len(args) < 2 || args[0] != "db" {
		return usageError()
	}

	switch args[1] {
	case "connect":
		return runDBConnect(args[2:])
	case "persist-project-response":
		return runPersistProjectResponse(args[2:])
	default:
		return usageError()
	}
}

func usageError() error {
	return fmt.Errorf("usage:\n  einar db connect [--project <name|id>] [--port <n>] [--api <url>] [--token <jwt>]\n  einar db persist-project-response [--file <response.json>]")
}

func runDBConnect(args []string) error {
	fs := flag.NewFlagSet("db connect", flag.ContinueOnError)
	api := fs.String("api", envOrDefault("EINAR_DB_API", defaultDBAPI), "Postgres API base URL")
	project := fs.String("project", "", "project name or id (defaults to .einar/config.json)")
	host := fs.String("host", "127.0.0.1", "local host for tunnel listener")
	port := fs.Int("port", 15432, "local port for tunnel listener")
	token := fs.String("token", envOrDefault("EINAR_TOKEN", os.Getenv("PGTUNNEL_TOKEN")), "Bearer token")
	devSub := fs.String("dev-sub", envOrDefault("EINAR_DEV_SUB", "dev-user"), "X-Dev-Sub when AUTH is disabled")
	insecure := fs.Bool("insecure-unauthenticated-local-test", false, "omit Authorization header for local test only")

	if err := fs.Parse(args); err != nil {
		return err
	}
	if *port <= 0 || *port > 65535 {
		return fmt.Errorf("--port must be between 1 and 65535")
	}

	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("cannot determine working directory: %w", err)
	}

	cfgPath := filepath.Join(cwd, ".einar", "config.json")
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		return err
	}

	projectRef := strings.TrimSpace(*project)
	if projectRef == "" {
		projectRef = firstNonEmpty(cfg.ProjectName, cfg.ProjectID)
	}
	if projectRef == "" {
		return fmt.Errorf("missing project; use --project or persist project in %s", cfgPath)
	}

	resolvedToken := strings.TrimSpace(*token)
	if resolvedToken == "" && !*insecure {
		resolvedToken, err = resolveToken(cwd)
		if err != nil {
			return err
		}
	}

	listenAddr := net.JoinHostPort(*host, strconv.Itoa(*port))
	localDatabaseURL, err := tunnelDatabaseURL(cfg.DatabaseURL, *host, *port)
	if err != nil {
		return fmt.Errorf("cannot prepare local database URL: %w", err)
	}

	wsURL, err := tunnel.TunnelURL(*api, projectRef)
	if err != nil {
		return err
	}

	log.Printf("project: %s", projectRef)
	log.Printf("tunnel: %s -> %s", listenAddr, wsURL)
	if localDatabaseURL != "" {
		log.Printf("database url (local): %s", localDatabaseURL)
	}
	if cfg.DatabaseSchema != "" {
		log.Printf("schema: %s (informational only; databaseUrl remains the source of truth)", cfg.DatabaseSchema)
	}
	log.Printf("ready: keep this process running while you use psql/DBeaver")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return tunnel.Serve(ctx, listenAddr, tunnel.Options{
		APIBaseURL:   *api,
		Project:      projectRef,
		Token:        resolvedToken,
		DevSub:       *devSub,
		InsecureAuth: *insecure,
	}, log.Printf)
}

type authConfigFile struct {
	Token           string `json:"token"`
	AccessToken     string `json:"accessToken"`
	ProjectAPIToken string `json:"ProjectAPIToken"`
}

type createProjectResponse struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Schema      string `json:"schema"`
	DatabaseURL string `json:"databaseUrl"`
}

type projectConfig struct {
	ProjectID      string
	ProjectName    string
	DatabaseURL    string
	DatabaseSchema string
}

type configFile struct {
	Project struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"project"`
	Database struct {
		URL    string `json:"url"`
		Schema string `json:"schema"`
	} `json:"database"`

	LastProjectID   string `json:"lastProjectId"`
	LastProjectSlug string `json:"lastProjectSlug"`

	ProjectDbName     string `json:"projectDbName"`
	ProjectDbUser     string `json:"projectDbUser"`
	ProjectDbPassword string `json:"projectDbPassword"`
	ProjectDbHost     string `json:"projectDbHost"`
	ProjectDbPort     int    `json:"projectDbPort"`
}

func runPersistProjectResponse(args []string) error {
	fs := flag.NewFlagSet("db persist-project-response", flag.ContinueOnError)
	file := fs.String("file", "", "path to JSON response from POST /projects (defaults to stdin)")

	if err := fs.Parse(args); err != nil {
		return err
	}

	var raw []byte
	var err error
	if strings.TrimSpace(*file) != "" {
		raw, err = os.ReadFile(*file)
		if err != nil {
			return fmt.Errorf("cannot read %s: %w", *file, err)
		}
	} else {
		raw, err = io.ReadAll(os.Stdin)
		if err != nil {
			return fmt.Errorf("cannot read stdin: %w", err)
		}
	}

	var resp createProjectResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return fmt.Errorf("invalid project response JSON: %w", err)
	}

	if strings.TrimSpace(resp.ID) == "" || strings.TrimSpace(resp.Name) == "" || strings.TrimSpace(resp.DatabaseURL) == "" {
		return errors.New("project response must include id, name and databaseUrl")
	}

	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("cannot determine working directory: %w", err)
	}

	cfgPath := filepath.Join(cwd, ".einar", "config.json")
	if err := persistProjectConfig(cfgPath, resp); err != nil {
		return err
	}

	log.Printf("persisted project config: %s", cfgPath)
	log.Printf("project: %s (%s)", strings.TrimSpace(resp.Name), strings.TrimSpace(resp.ID))
	if strings.TrimSpace(resp.Schema) != "" {
		log.Printf("schema: %s", strings.TrimSpace(resp.Schema))
	}
	log.Printf("database url: %s", strings.TrimSpace(resp.DatabaseURL))
	log.Printf("next: einar db connect")
	return nil
}

func loadConfig(path string) (projectConfig, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return projectConfig{}, fmt.Errorf("missing %s", path)
		}
		return projectConfig{}, fmt.Errorf("cannot read %s: %w", path, err)
	}

	var parsed configFile
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return projectConfig{}, fmt.Errorf("invalid JSON in %s: %w", path, err)
	}

	databaseURL := strings.TrimSpace(parsed.Database.URL)
	if databaseURL == "" {
		legacyURL, err := buildLegacyDatabaseURL(parsed)
		if err != nil {
			return projectConfig{}, err
		}
		databaseURL = legacyURL
	}

	return projectConfig{
		ProjectID:      firstNonEmpty(strings.TrimSpace(parsed.Project.ID), strings.TrimSpace(parsed.LastProjectID)),
		ProjectName:    firstNonEmpty(strings.TrimSpace(parsed.Project.Name), strings.TrimSpace(parsed.LastProjectSlug)),
		DatabaseURL:    databaseURL,
		DatabaseSchema: strings.TrimSpace(parsed.Database.Schema),
	}, nil
}

func persistProjectConfig(path string, resp createProjectResponse) error {
	cfg, err := readConfigDocument(path)
	if err != nil {
		return err
	}

	cfg["project"] = map[string]any{
		"id":   strings.TrimSpace(resp.ID),
		"name": strings.TrimSpace(resp.Name),
	}

	database := map[string]any{
		"url": strings.TrimSpace(resp.DatabaseURL),
	}
	if schema := strings.TrimSpace(resp.Schema); schema != "" {
		database["schema"] = schema
	}
	cfg["database"] = database
	cfg["configVersion"] = 1

	encoded, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("cannot encode %s: %w", path, err)
	}
	encoded = append(encoded, '\n')

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("cannot create %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		return fmt.Errorf("cannot write %s: %w", path, err)
	}
	return nil
}

func readConfigDocument(path string) (map[string]any, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]any{}, nil
		}
		return nil, fmt.Errorf("cannot read %s: %w", path, err)
	}

	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return map[string]any{}, nil
	}

	var cfg map[string]any
	if err := json.Unmarshal(trimmed, &cfg); err != nil {
		return nil, fmt.Errorf("invalid JSON in %s: %w", path, err)
	}
	if cfg == nil {
		cfg = map[string]any{}
	}
	return cfg, nil
}

func buildLegacyDatabaseURL(cfg configFile) (string, error) {
	name := strings.TrimSpace(cfg.ProjectDbName)
	user := strings.TrimSpace(cfg.ProjectDbUser)
	password := strings.TrimSpace(cfg.ProjectDbPassword)
	if name == "" || user == "" || password == "" {
		return "", nil
	}

	host := strings.TrimSpace(cfg.ProjectDbHost)
	if host == "" {
		host = "db"
	}

	port := cfg.ProjectDbPort
	if port == 0 {
		port = 5432
	}

	u := &url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(user, password),
		Host:   net.JoinHostPort(host, strconv.Itoa(port)),
		Path:   "/" + name,
	}

	q := u.Query()
	if _, ok := q["sslmode"]; !ok {
		q.Set("sslmode", "disable")
	}
	u.RawQuery = q.Encode()

	return u.String(), nil
}

func tunnelDatabaseURL(rawURL, host string, port int) (string, error) {
	if strings.TrimSpace(rawURL) == "" {
		return "", nil
	}

	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	u.Host = net.JoinHostPort(host, strconv.Itoa(port))

	q := u.Query()
	if _, ok := q["sslmode"]; !ok {
		q.Set("sslmode", "disable")
	}
	u.RawQuery = q.Encode()

	return u.String(), nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func envOrDefault(key, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return strings.TrimSpace(fallback)
	}
	return value
}

func resolveToken(cwd string) (string, error) {
	envToken := strings.TrimSpace(firstNonEmpty(os.Getenv("EINAR_TOKEN"), os.Getenv("PGTUNNEL_TOKEN")))
	if envToken != "" {
		return envToken, nil
	}

	candidates := make([]string, 0, 8)
	for dir := cwd; ; {
		candidates = append(candidates, filepath.Join(dir, ".einar", "config.json"))
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}

	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".einar", "config.json"))
	}

	seen := map[string]struct{}{}
	for _, candidate := range candidates {
		if _, ok := seen[candidate]; ok {
			continue
		}
		seen[candidate] = struct{}{}

		token, err := readTokenFromConfig(candidate)
		if err != nil {
			continue
		}
		if token != "" {
			return token, nil
		}
	}

	return "", errors.New("missing token: run `einar login` or pass --token")
}

func readTokenFromConfig(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}

	var parsed authConfigFile
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", err
	}

	return strings.TrimSpace(firstNonEmpty(parsed.Token, parsed.AccessToken, parsed.ProjectAPIToken)), nil
}
