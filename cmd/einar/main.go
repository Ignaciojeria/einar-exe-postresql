package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
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

const defaultDBAPI = "https://postgresql.exe.xyz"

func main() {
	if err := run(os.Args[1:]); err != nil {
		log.Fatal(err)
	}
}

func run(args []string) error {
	if len(args) < 2 || args[0] != "db" || args[1] != "connect" {
		return fmt.Errorf("usage: einar db connect [--project <name|id>] [--port <n>] [--api <url>] [--token <jwt>]")
	}

	return runDBConnect(args[2:])
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

type projectConfig struct {
	ProjectID   string
	ProjectName string
	DatabaseURL string
}

type configFile struct {
	Project struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"project"`
	Database struct {
		URL string `json:"url"`
	} `json:"database"`

	LastProjectID   string `json:"lastProjectId"`
	LastProjectSlug string `json:"lastProjectSlug"`

	ProjectDbName     string `json:"projectDbName"`
	ProjectDbUser     string `json:"projectDbUser"`
	ProjectDbPassword string `json:"projectDbPassword"`
	ProjectDbHost     string `json:"projectDbHost"`
	ProjectDbPort     int    `json:"projectDbPort"`
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
		ProjectID:   firstNonEmpty(strings.TrimSpace(parsed.Project.ID), strings.TrimSpace(parsed.LastProjectID)),
		ProjectName: firstNonEmpty(strings.TrimSpace(parsed.Project.Name), strings.TrimSpace(parsed.LastProjectSlug)),
		DatabaseURL: databaseURL,
	}, nil
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
