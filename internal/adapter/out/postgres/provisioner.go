package postgres

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/Ignaciojeria/ioc"
)

var _ = ioc.Register(NewProvisioner)

var identifierRegex = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

type Provisioner struct {
	adminDatabase string
	publicHost    string
	publicPort    string
}

type ProvisionRequest struct {
	ID       string
	OwnerSub string
	Name     string
	DBName   string
	DBUser   string
	Password string
}

type ProvisionResult struct {
	DatabaseURL string
}

type Project struct {
	ID          string
	OwnerSub    string
	Name        string
	DBName      string
	DBUser      string
	DatabaseURL string
}

type ProjectConflictError struct {
	Detail string
}

func (e ProjectConflictError) Error() string { return e.Detail }

type provisionError struct {
	msg string
	err error
}

func (e provisionError) Error() string {
	if e.err == nil {
		return e.msg
	}
	return e.msg + ": " + e.err.Error()
}

func (e provisionError) Unwrap() error { return e.err }

func NewProvisioner() (*Provisioner, error) {
	p := &Provisioner{
		adminDatabase: envOrDefault("POSTGRES_ADMIN_DATABASE", "postgres"),
		publicHost:    envOrDefault("POSTGRES_PUBLIC_HOST", "localhost"),
		publicPort:    envOrDefault("POSTGRES_PUBLIC_PORT", "5432"),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := p.ensureMetadataTable(ctx); err != nil {
		return nil, err
	}

	return p, nil
}

func (p *Provisioner) FindProjectForOwner(ctx context.Context, ownerSub, idOrName string) (Project, bool, error) {
	out, err := p.psql(ctx, p.adminDatabase, fmt.Sprintf(`
SELECT id || E'\t' || owner_sub || E'\t' || name || E'\t' || db_name || E'\t' || db_user || E'\t' || database_url
FROM public.projects
WHERE owner_sub = %s AND (id::text = %s OR name = %s)
LIMIT 1;
`, sqlLiteral(ownerSub), sqlLiteral(idOrName), sqlLiteral(idOrName)))
	if err != nil {
		return Project{}, false, provisionError{msg: "cannot find project", err: err}
	}
	line := strings.TrimSpace(out)
	if line == "" {
		return Project{}, false, nil
	}
	parts := strings.Split(line, "	")
	if len(parts) != 6 {
		return Project{}, false, provisionError{msg: "invalid project metadata result"}
	}
	return Project{
		ID:          parts[0],
		OwnerSub:    parts[1],
		Name:        parts[2],
		DBName:      parts[3],
		DBUser:      parts[4],
		DatabaseURL: parts[5],
	}, true, nil
}

func (p *Provisioner) Provision(ctx context.Context, req ProvisionRequest) (ProvisionResult, error) {
	if err := validateProvisionRequest(req); err != nil {
		return ProvisionResult{}, err
	}

	lockKey := req.OwnerSub + ":" + req.Name
	if _, err := p.psql(ctx, p.adminDatabase, fmt.Sprintf(`SELECT pg_advisory_lock(hashtextextended(%s, 0));`, sqlLiteral(lockKey))); err != nil {
		return ProvisionResult{}, provisionError{msg: "cannot acquire project provisioning lock", err: err}
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = p.psql(unlockCtx, p.adminDatabase, fmt.Sprintf(`SELECT pg_advisory_unlock(hashtextextended(%s, 0));`, sqlLiteral(lockKey)))
	}()

	conflict, err := p.conflict(ctx, req)
	if err != nil {
		return ProvisionResult{}, err
	}
	if conflict != "" {
		return ProvisionResult{}, ProjectConflictError{Detail: conflict}
	}

	databaseURL := buildDatabaseURL(p.publicHost, p.publicPort, req.DBUser, req.Password, req.DBName)
	createdRole := false
	createdDB := false

	if _, err := p.psql(ctx, p.adminDatabase, fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD %s;`, sqlIdentifier(req.DBUser), sqlLiteral(req.Password))); err != nil {
		return ProvisionResult{}, provisionError{msg: "cannot create database role", err: err}
	}
	createdRole = true

	if _, err := p.psql(ctx, p.adminDatabase, fmt.Sprintf(`CREATE DATABASE %s OWNER %s;`, sqlIdentifier(req.DBName), sqlIdentifier(req.DBUser))); err != nil {
		p.cleanup(ctx, req, createdDB, createdRole)
		return ProvisionResult{}, provisionError{msg: "cannot create database", err: err}
	}
	createdDB = true

	if _, err := p.psql(ctx, req.DBName, fmt.Sprintf(`
GRANT CONNECT ON DATABASE %s TO %s;
GRANT USAGE, CREATE ON SCHEMA public TO %s;
`, sqlIdentifier(req.DBName), sqlIdentifier(req.DBUser), sqlIdentifier(req.DBUser))); err != nil {
		p.cleanup(ctx, req, createdDB, createdRole)
		return ProvisionResult{}, provisionError{msg: "cannot grant database permissions", err: err}
	}

	if _, err := p.psql(ctx, p.adminDatabase, fmt.Sprintf(`
INSERT INTO public.projects (id, owner_sub, name, db_name, db_user, database_url)
VALUES (%s, %s, %s, %s, %s, %s);
`, sqlLiteral(req.ID), sqlLiteral(req.OwnerSub), sqlLiteral(req.Name), sqlLiteral(req.DBName), sqlLiteral(req.DBUser), sqlLiteral(databaseURL))); err != nil {
		p.cleanup(ctx, req, createdDB, createdRole)
		if isUniqueViolation(err) {
			return ProvisionResult{}, ProjectConflictError{Detail: "project already exists"}
		}
		return ProvisionResult{}, provisionError{msg: "cannot persist project metadata", err: err}
	}

	return ProvisionResult{DatabaseURL: databaseURL}, nil
}

func (p *Provisioner) ensureMetadataTable(ctx context.Context) error {
	_, err := p.psql(ctx, p.adminDatabase, `
CREATE TABLE IF NOT EXISTS public.projects (
	id uuid PRIMARY KEY,
	owner_sub text NOT NULL,
	name text NOT NULL,
	db_name text NOT NULL UNIQUE,
	db_user text NOT NULL UNIQUE,
	database_url text NOT NULL,
	created_at timestamptz NOT NULL DEFAULT now(),
	UNIQUE (owner_sub, name)
);
`)
	if err != nil {
		return provisionError{msg: "cannot ensure projects metadata table", err: err}
	}
	return nil
}

func (p *Provisioner) conflict(ctx context.Context, req ProvisionRequest) (string, error) {
	out, err := p.psql(ctx, p.adminDatabase, fmt.Sprintf(`
SELECT CASE
	WHEN EXISTS (SELECT 1 FROM public.projects WHERE owner_sub = %s AND name = %s) THEN 'project already exists for owner'
	WHEN EXISTS (SELECT 1 FROM pg_database WHERE datname = %s) THEN 'database already exists'
	WHEN EXISTS (SELECT 1 FROM pg_roles WHERE rolname = %s) THEN 'database role already exists'
	ELSE ''
END;
`, sqlLiteral(req.OwnerSub), sqlLiteral(req.Name), sqlLiteral(req.DBName), sqlLiteral(req.DBUser)))
	if err != nil {
		return "", provisionError{msg: "cannot check project conflicts", err: err}
	}
	return strings.TrimSpace(out), nil
}

func (p *Provisioner) cleanup(ctx context.Context, req ProvisionRequest, createdDB, createdRole bool) {
	cleanupCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if createdDB {
		_, _ = p.psql(cleanupCtx, p.adminDatabase, fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE);`, sqlIdentifier(req.DBName)))
	}
	if createdRole {
		_, _ = p.psql(cleanupCtx, p.adminDatabase, fmt.Sprintf(`DROP ROLE IF EXISTS %s;`, sqlIdentifier(req.DBUser)))
	}
}

func (p *Provisioner) psql(ctx context.Context, database string, script string) (string, error) {
	args := []string{"-n", "-u", "postgres", "psql", "-X", "-v", "ON_ERROR_STOP=1", "-At", "-d", database}
	cmd := exec.CommandContext(ctx, "sudo", args...)
	cmd.Stdin = strings.NewReader(script)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		if msg == "" {
			msg = err.Error()
		}
		return stdout.String(), errors.New(msg)
	}
	return stdout.String(), nil
}

func validateProvisionRequest(req ProvisionRequest) error {
	for _, candidate := range []struct {
		name  string
		value string
	}{
		{name: "db name", value: req.DBName},
		{name: "db user", value: req.DBUser},
	} {
		if !identifierRegex.MatchString(candidate.value) {
			return fmt.Errorf("invalid %s %q", candidate.name, candidate.value)
		}
	}
	if strings.TrimSpace(req.ID) == "" || strings.TrimSpace(req.OwnerSub) == "" || strings.TrimSpace(req.Name) == "" || req.Password == "" {
		return errors.New("missing provisioning data")
	}
	return nil
}

func buildDatabaseURL(host, port, user, password, database string) string {
	u := &url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(user, password),
		Host:   net.JoinHostPort(host, port),
		Path:   "/" + database,
	}
	q := u.Query()
	q.Set("sslmode", "disable")
	u.RawQuery = q.Encode()
	return u.String()
}

func sqlIdentifier(identifier string) string {
	return `"` + strings.ReplaceAll(identifier, `"`, `""`) + `"`
}

func sqlLiteral(value string) string {
	return `'` + strings.ReplaceAll(value, `'`, `''`) + `'`
}

func envOrDefault(key, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	return value
}

func isUniqueViolation(err error) bool {
	return strings.Contains(err.Error(), "duplicate key value violates unique constraint")
}
