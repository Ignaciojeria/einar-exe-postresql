package postgres

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Ignaciojeria/ioc"
)

var _ = ioc.Register(NewProvisioner)

var identifierRegex = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)
var containerNameRegex = regexp.MustCompile(`[^a-zA-Z0-9_.-]+`)

type Provisioner struct {
	adminDatabase           string
	sharedDatabase          string
	publicHost              string
	publicPort              string
	provisioningModel       string
	electricEnabled         bool
	electricPortBase        int
	electricGlobalPort      int
	electricImage           string
	electricStorageBase     string
	electricGlobalContainer string
	electricGlobalSecret    string
	electricPGUser          string
	electricPGPassword      string
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
	ID                string
	OwnerSub          string
	Name              string
	DBName            string
	DBSchema          string
	DBUser            string
	DatabaseURL       string
	ElectricPort      int
	ElectricSecret    string
	ElectricStatus    string
	ElectricContainer string
}

func (p Project) ElectricURL() string {
	if p.ElectricPort <= 0 {
		return ""
	}
	return fmt.Sprintf("http://127.0.0.1:%d", p.ElectricPort)
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
	adminDatabase := envOrDefault("POSTGRES_ADMIN_DATABASE", "postgres")
	globalSecret := strings.TrimSpace(os.Getenv("ELECTRIC_GLOBAL_SECRET"))
	if globalSecret == "" {
		globalSecret = strings.TrimSpace(os.Getenv("ELECTRIC_SECRET"))
	}
	p := &Provisioner{
		adminDatabase:           adminDatabase,
		sharedDatabase:          envOrDefault("POSTGRES_SHARED_DATABASE", adminDatabase),
		publicHost:              envOrDefault("POSTGRES_PUBLIC_HOST", "localhost"),
		publicPort:              envOrDefault("POSTGRES_PUBLIC_PORT", "5432"),
		provisioningModel:       strings.ToLower(envOrDefault("POSTGRES_PROVISIONING_MODEL", "schema")),
		electricEnabled:         envOrDefault("ELECTRIC_AUTO_PROVISION", "true") != "false",
		electricPortBase:        envIntOrDefault("ELECTRIC_PORT_BASE", 13100),
		electricGlobalPort:      envIntOrDefault("ELECTRIC_GLOBAL_PORT", envIntOrDefault("ELECTRIC_PORT_BASE", 13100)),
		electricImage:           envOrDefault("ELECTRIC_DOCKER_IMAGE", "electricsql/electric:latest"),
		electricStorageBase:     envOrDefault("ELECTRIC_PROJECT_STORAGE_BASE", "/var/lib/electric/projects"),
		electricGlobalContainer: envOrDefault("ELECTRIC_GLOBAL_CONTAINER", "electric-shared"),
		electricGlobalSecret:    globalSecret,
		electricPGUser:          envOrDefault("ELECTRIC_PG_USER", "electric"),
		electricPGPassword:      strings.TrimSpace(os.Getenv("ELECTRIC_PG_PASSWORD")),
	}
	if p.electricPGPassword == "" {
		p.electricPGPassword = electricPasswordFromEnvFile("/etc/electric/electric.env")
	}
	if p.provisioningModel != "schema" && p.provisioningModel != "database" {
		return nil, fmt.Errorf("POSTGRES_PROVISIONING_MODEL must be schema or database")
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
SELECT id || E'\t' || owner_sub || E'\t' || name || E'\t' || db_name || E'\t' || COALESCE(db_schema, db_name) || E'\t' || db_user || E'\t' || database_url || E'\t' || COALESCE(electric_port::text, '') || E'\t' || COALESCE(electric_secret, '') || E'\t' || COALESCE(electric_status, '') || E'\t' || COALESCE(electric_container, '')
FROM public.projects
WHERE owner_sub = %s AND (id::text = %s OR name = %s)
LIMIT 1;
`, sqlLiteral(ownerSub), sqlLiteral(idOrName), sqlLiteral(idOrName)))
	if err != nil {
		return Project{}, false, provisionError{msg: "cannot find project", err: err}
	}
	line := strings.TrimRight(out, "\r\n")
	if strings.TrimSpace(line) == "" {
		return Project{}, false, nil
	}
	parts := strings.Split(line, "\t")
	if len(parts) != 11 {
		return Project{}, false, provisionError{msg: "invalid project metadata result"}
	}
	electricPort, _ := strconv.Atoi(parts[7])
	return Project{
		ID:                parts[0],
		OwnerSub:          parts[1],
		Name:              parts[2],
		DBName:            parts[3],
		DBSchema:          parts[4],
		DBUser:            parts[5],
		DatabaseURL:       parts[6],
		ElectricPort:      electricPort,
		ElectricSecret:    parts[8],
		ElectricStatus:    parts[9],
		ElectricContainer: parts[10],
	}, true, nil
}

func (p *Provisioner) Provision(ctx context.Context, req ProvisionRequest) (ProvisionResult, error) {
	if err := validateProvisionRequest(req); err != nil {
		return ProvisionResult{}, err
	}
	if p.provisioningModel == "database" {
		return p.provisionDatabase(ctx, req)
	}
	return p.provisionSchema(ctx, req)
}

func (p *Provisioner) provisionSchema(ctx context.Context, req ProvisionRequest) (ProvisionResult, error) {
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

	schemaName := req.DBName
	databaseURL := buildDatabaseURL(p.publicHost, p.publicPort, req.DBUser, req.Password, p.sharedDatabase)
	electricSecret := p.electricGlobalSecret
	if electricSecret == "" {
		electricSecret = p.sharedElectricSecret(ctx)
	}
	if electricSecret == "" {
		electricSecret, err = randomHex(32)
		if err != nil {
			return ProvisionResult{}, provisionError{msg: "cannot generate electric secret", err: err}
		}
		p.electricGlobalSecret = electricSecret
	}

	createdRole := false
	createdSchema := false
	createdMetadata := false

	if _, err := p.psql(ctx, p.adminDatabase, fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD %s;`, sqlIdentifier(req.DBUser), sqlLiteral(req.Password))); err != nil {
		return ProvisionResult{}, provisionError{msg: "cannot create database role", err: err}
	}
	createdRole = true

	if _, err := p.psql(ctx, p.sharedDatabase, fmt.Sprintf(`
BEGIN;
CREATE SCHEMA %s AUTHORIZATION %s;
REVOKE ALL ON SCHEMA public FROM PUBLIC;
REVOKE ALL ON SCHEMA %s FROM PUBLIC;
GRANT CONNECT ON DATABASE %s TO %s;
GRANT USAGE, CREATE ON SCHEMA %s TO %s;
ALTER ROLE %s SET search_path = %s;
GRANT USAGE ON SCHEMA %s TO %s;
GRANT SELECT ON ALL TABLES IN SCHEMA %s TO %s;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA %s TO %s;
ALTER DEFAULT PRIVILEGES FOR ROLE %s IN SCHEMA %s GRANT SELECT ON TABLES TO %s;
ALTER DEFAULT PRIVILEGES FOR ROLE %s IN SCHEMA %s GRANT USAGE, SELECT ON SEQUENCES TO %s;
COMMIT;
`,
		sqlIdentifier(schemaName), sqlIdentifier(req.DBUser),
		sqlIdentifier(schemaName),
		sqlIdentifier(p.sharedDatabase), sqlIdentifier(req.DBUser),
		sqlIdentifier(schemaName), sqlIdentifier(req.DBUser),
		sqlIdentifier(req.DBUser), sqlIdentifier(schemaName),
		sqlIdentifier(schemaName), sqlIdentifier(p.electricPGUser),
		sqlIdentifier(schemaName), sqlIdentifier(p.electricPGUser),
		sqlIdentifier(schemaName), sqlIdentifier(p.electricPGUser),
		sqlIdentifier(req.DBUser), sqlIdentifier(schemaName), sqlIdentifier(p.electricPGUser),
		sqlIdentifier(req.DBUser), sqlIdentifier(schemaName), sqlIdentifier(p.electricPGUser),
	)); err != nil {
		p.cleanupSchema(ctx, req, createdSchema, createdRole, createdMetadata)
		return ProvisionResult{}, provisionError{msg: "cannot create tenant schema", err: err}
	}
	createdSchema = true

	if _, err := p.psql(ctx, p.adminDatabase, fmt.Sprintf(`
INSERT INTO public.projects (id, owner_sub, name, db_name, db_schema, db_user, database_url, electric_port, electric_secret, electric_container, electric_status)
VALUES (%s, %s, %s, %s, %s, %s, %s, %d, %s, %s, 'shared');
`, sqlLiteral(req.ID), sqlLiteral(req.OwnerSub), sqlLiteral(req.Name), sqlLiteral(p.sharedDatabase), sqlLiteral(schemaName), sqlLiteral(req.DBUser), sqlLiteral(databaseURL), p.electricGlobalPort, sqlLiteral(electricSecret), sqlLiteral(p.electricGlobalContainer))); err != nil {
		p.cleanupSchema(ctx, req, createdSchema, createdRole, createdMetadata)
		if isUniqueViolation(err) {
			return ProvisionResult{}, ProjectConflictError{Detail: "project already exists"}
		}
		return ProvisionResult{}, provisionError{msg: "cannot persist project metadata", err: err}
	}
	createdMetadata = true

	if p.electricEnabled {
		project := Project{ID: req.ID, Name: req.Name, DBName: p.sharedDatabase, DBSchema: schemaName, DBUser: req.DBUser, DatabaseURL: databaseURL, ElectricPort: p.electricGlobalPort, ElectricSecret: electricSecret, ElectricContainer: p.electricGlobalContainer}
		if _, err := p.EnsureElectric(ctx, project); err != nil {
			p.cleanupSchema(ctx, req, createdSchema, createdRole, createdMetadata)
			return ProvisionResult{}, provisionError{msg: "cannot ensure shared electric sync", err: err}
		}
	}

	return ProvisionResult{DatabaseURL: databaseURL}, nil
}

func (p *Provisioner) provisionDatabase(ctx context.Context, req ProvisionRequest) (ProvisionResult, error) {
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
	electricPort, err := p.nextElectricPort(ctx)
	if err != nil {
		return ProvisionResult{}, err
	}
	electricSecret, err := randomHex(32)
	if err != nil {
		return ProvisionResult{}, provisionError{msg: "cannot generate electric secret", err: err}
	}
	electricContainer := electricContainerName(req.DBName)

	createdRole := false
	createdDB := false
	createdMetadata := false

	if _, err := p.psql(ctx, p.adminDatabase, fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD %s;`, sqlIdentifier(req.DBUser), sqlLiteral(req.Password))); err != nil {
		return ProvisionResult{}, provisionError{msg: "cannot create database role", err: err}
	}
	createdRole = true

	if _, err := p.psql(ctx, p.adminDatabase, fmt.Sprintf(`CREATE DATABASE %s OWNER %s;`, sqlIdentifier(req.DBName), sqlIdentifier(req.DBUser))); err != nil {
		p.cleanup(ctx, req, createdDB, createdRole, createdMetadata, electricContainer)
		return ProvisionResult{}, provisionError{msg: "cannot create database", err: err}
	}
	createdDB = true

	if _, err := p.psql(ctx, req.DBName, fmt.Sprintf(`
GRANT CONNECT ON DATABASE %s TO %s;
GRANT USAGE, CREATE ON SCHEMA public TO %s;
`, sqlIdentifier(req.DBName), sqlIdentifier(req.DBUser), sqlIdentifier(req.DBUser))); err != nil {
		p.cleanup(ctx, req, createdDB, createdRole, createdMetadata, electricContainer)
		return ProvisionResult{}, provisionError{msg: "cannot grant database permissions", err: err}
	}

	if _, err := p.psql(ctx, p.adminDatabase, fmt.Sprintf(`
INSERT INTO public.projects (id, owner_sub, name, db_name, db_schema, db_user, database_url, electric_port, electric_secret, electric_container, electric_status)
VALUES (%s, %s, %s, %s, %s, %s, %s, %d, %s, %s, 'provisioning');
`, sqlLiteral(req.ID), sqlLiteral(req.OwnerSub), sqlLiteral(req.Name), sqlLiteral(req.DBName), sqlLiteral("public"), sqlLiteral(req.DBUser), sqlLiteral(databaseURL), electricPort, sqlLiteral(electricSecret), sqlLiteral(electricContainer))); err != nil {
		p.cleanup(ctx, req, createdDB, createdRole, createdMetadata, electricContainer)
		if isUniqueViolation(err) {
			return ProvisionResult{}, ProjectConflictError{Detail: "project already exists"}
		}
		return ProvisionResult{}, provisionError{msg: "cannot persist project metadata", err: err}
	}
	createdMetadata = true

	if p.electricEnabled {
		if err := p.startElectric(ctx, req.DBName, electricPort, electricSecret, electricContainer); err != nil {
			p.cleanup(ctx, req, createdDB, createdRole, createdMetadata, electricContainer)
			return ProvisionResult{}, provisionError{msg: "cannot start electric sync", err: err}
		}
		if _, err := p.psql(ctx, p.adminDatabase, fmt.Sprintf(`UPDATE public.projects SET electric_status = 'running' WHERE id = %s;`, sqlLiteral(req.ID))); err != nil {
			return ProvisionResult{}, provisionError{msg: "cannot update electric status", err: err}
		}
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
ALTER TABLE public.projects ADD COLUMN IF NOT EXISTS db_schema text;
ALTER TABLE public.projects ADD COLUMN IF NOT EXISTS electric_port integer;
ALTER TABLE public.projects ADD COLUMN IF NOT EXISTS electric_secret text;
ALTER TABLE public.projects ADD COLUMN IF NOT EXISTS electric_container text;
ALTER TABLE public.projects ADD COLUMN IF NOT EXISTS electric_status text NOT NULL DEFAULT 'not_provisioned';
ALTER TABLE public.projects ADD COLUMN IF NOT EXISTS electric_created_at timestamptz;
ALTER TABLE public.projects DROP CONSTRAINT IF EXISTS projects_db_name_key;
ALTER TABLE public.projects DROP CONSTRAINT IF EXISTS projects_electric_port_key;
ALTER TABLE public.projects DROP CONSTRAINT IF EXISTS projects_electric_container_key;
UPDATE public.projects SET db_schema = 'public' WHERE db_schema IS NULL;
DROP INDEX IF EXISTS public.projects_db_schema_key;
CREATE UNIQUE INDEX IF NOT EXISTS projects_db_name_schema_key ON public.projects (db_name, db_schema);
`)
	if err != nil {
		return provisionError{msg: "cannot ensure projects metadata table", err: err}
	}
	return nil
}

func (p *Provisioner) conflict(ctx context.Context, req ProvisionRequest) (string, error) {
	if p.provisioningModel == "schema" {
		out, err := p.psql(ctx, p.adminDatabase, fmt.Sprintf(`
SELECT CASE
	WHEN EXISTS (SELECT 1 FROM public.projects WHERE owner_sub = %s AND name = %s) THEN 'project already exists for owner'
	WHEN EXISTS (SELECT 1 FROM public.projects WHERE db_name = %s AND db_schema = %s) THEN 'schema already exists'
	WHEN EXISTS (SELECT 1 FROM pg_roles WHERE rolname = %s) THEN 'database role already exists'
	ELSE ''
END;
`, sqlLiteral(req.OwnerSub), sqlLiteral(req.Name), sqlLiteral(p.sharedDatabase), sqlLiteral(req.DBName), sqlLiteral(req.DBUser)))
		if err != nil {
			return "", provisionError{msg: "cannot check project conflicts", err: err}
		}
		conflict := strings.TrimSpace(out)
		if conflict != "" {
			return conflict, nil
		}
		out, err = p.psql(ctx, p.sharedDatabase, fmt.Sprintf(`
SELECT CASE
	WHEN EXISTS (SELECT 1 FROM information_schema.schemata WHERE schema_name = %s) THEN 'schema already exists'
	ELSE ''
END;
`, sqlLiteral(req.DBName)))
		if err != nil {
			return "", provisionError{msg: "cannot check schema conflicts", err: err}
		}
		return strings.TrimSpace(out), nil
	}

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

func (p *Provisioner) EnsureElectric(ctx context.Context, project Project) (Project, error) {
	if !p.electricEnabled {
		return project, errors.New("electric auto provisioning is disabled")
	}
	if p.provisioningModel == "schema" {
		return p.ensureSharedElectric(ctx, project)
	}
	if project.ElectricPort > 0 && project.ElectricSecret != "" && electricHealthy(ctx, project.ElectricPort) {
		return project, nil
	}

	lockKey := "electric:" + project.ID
	if _, err := p.psql(ctx, p.adminDatabase, fmt.Sprintf(`SELECT pg_advisory_lock(hashtextextended(%s, 0));`, sqlLiteral(lockKey))); err != nil {
		return project, provisionError{msg: "cannot acquire electric provisioning lock", err: err}
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = p.psql(unlockCtx, p.adminDatabase, fmt.Sprintf(`SELECT pg_advisory_unlock(hashtextextended(%s, 0));`, sqlLiteral(lockKey)))
	}()

	fresh, found, err := p.findProjectByID(ctx, project.ID)
	if err != nil {
		return project, err
	}
	if !found {
		return project, errors.New("project not found")
	}
	project = fresh
	if project.ElectricPort > 0 && project.ElectricSecret != "" && electricHealthy(ctx, project.ElectricPort) {
		return project, nil
	}

	if project.ElectricPort == 0 {
		port, err := p.nextElectricPort(ctx)
		if err != nil {
			return project, err
		}
		project.ElectricPort = port
	}
	if project.ElectricSecret == "" {
		secret, err := randomHex(32)
		if err != nil {
			return project, provisionError{msg: "cannot generate electric secret", err: err}
		}
		project.ElectricSecret = secret
	}
	if project.ElectricContainer == "" {
		project.ElectricContainer = electricContainerName(project.DBName)
	}

	if _, err := p.psql(ctx, p.adminDatabase, fmt.Sprintf(`
UPDATE public.projects
SET electric_port = %d,
    electric_secret = %s,
    electric_container = %s,
    electric_status = 'provisioning'
WHERE id = %s;
`, project.ElectricPort, sqlLiteral(project.ElectricSecret), sqlLiteral(project.ElectricContainer), sqlLiteral(project.ID))); err != nil {
		return project, provisionError{msg: "cannot update electric metadata", err: err}
	}

	if err := p.startElectric(ctx, project.DBName, project.ElectricPort, project.ElectricSecret, project.ElectricContainer); err != nil {
		_, _ = p.psql(context.Background(), p.adminDatabase, fmt.Sprintf(`UPDATE public.projects SET electric_status = 'error' WHERE id = %s;`, sqlLiteral(project.ID)))
		return project, err
	}
	if _, err := p.psql(ctx, p.adminDatabase, fmt.Sprintf(`UPDATE public.projects SET electric_status = 'running', electric_created_at = COALESCE(electric_created_at, now()) WHERE id = %s;`, sqlLiteral(project.ID))); err != nil {
		return project, provisionError{msg: "cannot update electric status", err: err}
	}
	project.ElectricStatus = "running"
	return project, nil
}

func (p *Provisioner) ensureSharedElectric(ctx context.Context, project Project) (Project, error) {
	port := p.electricGlobalPort
	secret := p.electricGlobalSecret
	if secret == "" {
		secret = project.ElectricSecret
	}
	if secret == "" {
		secret = p.sharedElectricSecret(ctx)
	}
	if secret == "" {
		generated, err := randomHex(32)
		if err != nil {
			return project, provisionError{msg: "cannot generate electric secret", err: err}
		}
		secret = generated
		p.electricGlobalSecret = generated
	}
	container := p.electricGlobalContainer
	if container == "" {
		container = "electric-shared"
	}

	if port > 0 && secret != "" && electricHealthy(ctx, port) {
		project.ElectricPort = port
		project.ElectricSecret = secret
		project.ElectricContainer = container
		project.ElectricStatus = "running"
		_, _ = p.psql(ctx, p.adminDatabase, fmt.Sprintf(`
UPDATE public.projects
SET electric_port = %d,
    electric_secret = %s,
    electric_container = %s,
    electric_status = 'running',
    electric_created_at = COALESCE(electric_created_at, now())
WHERE id = %s;
`, port, sqlLiteral(secret), sqlLiteral(container), sqlLiteral(project.ID)))
		return project, nil
	}

	lockKey := "electric:shared:"
	if _, err := p.psql(ctx, p.adminDatabase, fmt.Sprintf(`SELECT pg_advisory_lock(hashtextextended(%s, 0));`, sqlLiteral(lockKey))); err != nil {
		return project, provisionError{msg: "cannot acquire shared electric provisioning lock", err: err}
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = p.psql(unlockCtx, p.adminDatabase, fmt.Sprintf(`SELECT pg_advisory_unlock(hashtextextended(%s, 0));`, sqlLiteral(lockKey)))
	}()

	if electricHealthy(ctx, port) {
		project.ElectricPort = port
		project.ElectricSecret = secret
		project.ElectricContainer = container
		project.ElectricStatus = "running"
		return project, nil
	}

	if err := p.startElectric(ctx, p.sharedDatabase, port, secret, container); err != nil {
		_, _ = p.psql(context.Background(), p.adminDatabase, fmt.Sprintf(`UPDATE public.projects SET electric_status = 'error' WHERE id = %s;`, sqlLiteral(project.ID)))
		return project, err
	}
	if _, err := p.psql(ctx, p.adminDatabase, fmt.Sprintf(`
UPDATE public.projects
SET electric_port = %d,
    electric_secret = %s,
    electric_container = %s,
    electric_status = 'running',
    electric_created_at = COALESCE(electric_created_at, now())
WHERE electric_container = %s OR id = %s;
`, port, sqlLiteral(secret), sqlLiteral(container), sqlLiteral(container), sqlLiteral(project.ID))); err != nil {
		return project, provisionError{msg: "cannot update shared electric status", err: err}
	}
	project.ElectricPort = port
	project.ElectricSecret = secret
	project.ElectricContainer = container
	project.ElectricStatus = "running"
	return project, nil
}

func (p *Provisioner) findProjectByID(ctx context.Context, id string) (Project, bool, error) {
	out, err := p.psql(ctx, p.adminDatabase, fmt.Sprintf(`
SELECT id || E'\t' || owner_sub || E'\t' || name || E'\t' || db_name || E'\t' || COALESCE(db_schema, db_name) || E'\t' || db_user || E'\t' || database_url || E'\t' || COALESCE(electric_port::text, '') || E'\t' || COALESCE(electric_secret, '') || E'\t' || COALESCE(electric_status, '') || E'\t' || COALESCE(electric_container, '')
FROM public.projects
WHERE id = %s
LIMIT 1;
`, sqlLiteral(id)))
	if err != nil {
		return Project{}, false, provisionError{msg: "cannot find project", err: err}
	}
	line := strings.TrimRight(out, "\r\n")
	if strings.TrimSpace(line) == "" {
		return Project{}, false, nil
	}
	parts := strings.Split(line, "\t")
	if len(parts) != 11 {
		return Project{}, false, provisionError{msg: "invalid project metadata result"}
	}
	electricPort, _ := strconv.Atoi(parts[7])
	return Project{
		ID:                parts[0],
		OwnerSub:          parts[1],
		Name:              parts[2],
		DBName:            parts[3],
		DBSchema:          parts[4],
		DBUser:            parts[5],
		DatabaseURL:       parts[6],
		ElectricPort:      electricPort,
		ElectricSecret:    parts[8],
		ElectricStatus:    parts[9],
		ElectricContainer: parts[10],
	}, true, nil
}

func (p *Provisioner) sharedElectricSecret(ctx context.Context) string {
	out, err := p.psql(ctx, p.adminDatabase, fmt.Sprintf(`
SELECT electric_secret
FROM public.projects
WHERE electric_container = %s AND electric_secret IS NOT NULL AND electric_secret <> ''
ORDER BY electric_created_at NULLS LAST, created_at
LIMIT 1;
`, sqlLiteral(p.electricGlobalContainer)))
	if err != nil {
		return ""
	}
	secret := strings.TrimSpace(out)
	if secret != "" {
		p.electricGlobalSecret = secret
	}
	return secret
}

func (p *Provisioner) nextElectricPort(ctx context.Context) (int, error) {
	out, err := p.psql(ctx, p.adminDatabase, fmt.Sprintf(`
SELECT port
FROM generate_series(%d, %d) AS port
WHERE NOT EXISTS (SELECT 1 FROM public.projects WHERE electric_port = port)
ORDER BY port
LIMIT 1;
`, p.electricPortBase, p.electricPortBase+9999))
	if err != nil {
		return 0, provisionError{msg: "cannot allocate electric port", err: err}
	}
	port, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil || port == 0 {
		return 0, provisionError{msg: "cannot allocate electric port", err: err}
	}
	return port, nil
}

func (p *Provisioner) cleanup(ctx context.Context, req ProvisionRequest, createdDB, createdRole, createdMetadata bool, electricContainer string) {
	cleanupCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if electricContainer != "" {
		_ = dockerRm(cleanupCtx, electricContainer)
	}
	if createdMetadata {
		_, _ = p.psql(cleanupCtx, p.adminDatabase, fmt.Sprintf(`DELETE FROM public.projects WHERE id = %s;`, sqlLiteral(req.ID)))
	}
	if createdDB {
		_, _ = p.psql(cleanupCtx, p.adminDatabase, fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE);`, sqlIdentifier(req.DBName)))
	}
	if createdRole {
		_, _ = p.psql(cleanupCtx, p.adminDatabase, fmt.Sprintf(`DROP ROLE IF EXISTS %s;`, sqlIdentifier(req.DBUser)))
	}
}

func (p *Provisioner) cleanupSchema(ctx context.Context, req ProvisionRequest, createdSchema, createdRole, createdMetadata bool) {
	cleanupCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if createdMetadata {
		_, _ = p.psql(cleanupCtx, p.adminDatabase, fmt.Sprintf(`DELETE FROM public.projects WHERE id = %s;`, sqlLiteral(req.ID)))
	}
	if createdSchema {
		_, _ = p.psql(cleanupCtx, p.sharedDatabase, fmt.Sprintf(`DROP SCHEMA IF EXISTS %s CASCADE;`, sqlIdentifier(req.DBName)))
	}
	if createdRole {
		_, _ = p.psql(cleanupCtx, p.adminDatabase, fmt.Sprintf(`DROP ROLE IF EXISTS %s;`, sqlIdentifier(req.DBUser)))
	}
}

func (p *Provisioner) startElectric(ctx context.Context, dbName string, port int, secret, container string) error {
	if p.electricPGPassword == "" {
		return errors.New("ELECTRIC_PG_PASSWORD or /etc/electric/electric.env DATABASE_URL password is required")
	}
	storageDir := filepath.Join(p.electricStorageBase, dbName)
	if err := sudoInstallDir(ctx, storageDir); err != nil {
		return err
	}
	_ = dockerRm(ctx, container)
	databaseURL := buildDatabaseURL("host.docker.internal", "5432", p.electricPGUser, p.electricPGPassword, dbName)
	args := []string{
		"run", "-d",
		"--name", container,
		"--restart", "unless-stopped",
		"--add-host=host.docker.internal:host-gateway",
		"-p", fmt.Sprintf("127.0.0.1:%d:%d", port, port),
		"-e", "DATABASE_URL=" + databaseURL,
		"-e", fmt.Sprintf("ELECTRIC_PORT=%d", port),
		"-e", "ELECTRIC_STORAGE_DIR=/var/lib/electric",
		"-e", "ELECTRIC_USAGE_REPORTING=false",
		"-e", "ELECTRIC_SECRET=" + secret,
		"-e", "ELECTRIC_REPLICATION_STREAM_ID=" + replicationStreamID(dbName),
		"-v", storageDir + ":/var/lib/electric",
		p.electricImage,
	}
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker run: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return waitElectricReady(ctx, port)
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

func envIntOrDefault(key string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func isUniqueViolation(err error) bool {
	return strings.Contains(err.Error(), "duplicate key value violates unique constraint")
}

func randomHex(bytesLen int) (string, error) {
	b := make([]byte, bytesLen)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func electricContainerName(dbName string) string {
	name := "electric-" + containerNameRegex.ReplaceAllString(dbName, "-")
	if len(name) > 63 {
		return name[:63]
	}
	return name
}

func replicationStreamID(dbName string) string {
	id := "electric_" + strings.ReplaceAll(containerNameRegex.ReplaceAllString(dbName, "_"), "-", "_")
	if len(id) > 63 {
		return id[:63]
	}
	return id
}

func electricPasswordFromEnvFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "DATABASE_URL=") {
			raw := strings.Trim(strings.TrimPrefix(line, "DATABASE_URL="), `"'`)
			u, err := url.Parse(raw)
			if err != nil || u.User == nil {
				return ""
			}
			password, _ := u.User.Password()
			return password
		}
	}
	return ""
}

func sudoInstallDir(ctx context.Context, dir string) error {
	out, err := exec.CommandContext(ctx, "sudo", "install", "-d", "-m", "0750", "-o", "exedev", "-g", "exedev", dir).CombinedOutput()
	if err != nil {
		return fmt.Errorf("install storage dir: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func dockerRm(ctx context.Context, container string) error {
	out, err := exec.CommandContext(ctx, "docker", "rm", "-f", container).CombinedOutput()
	if err != nil && !strings.Contains(string(out), "No such container") {
		return fmt.Errorf("docker rm: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func waitElectricReady(ctx context.Context, port int) error {
	deadline := time.Now().Add(30 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		if electricHealthy(ctx, port) {
			return nil
		}
		lastErr = errors.New("not ready")
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return fmt.Errorf("electric did not become ready: %w", lastErr)
}

func electricHealthy(ctx context.Context, port int) bool {
	if port <= 0 {
		return false
	}
	client := http.Client{Timeout: 2 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/", port), nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode < 500
}
