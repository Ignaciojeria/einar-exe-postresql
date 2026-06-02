package in

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	postgresout "postgresql/internal/adapter/out/postgres"
	"postgresql/internal/shared/server"
	"postgresql/internal/shared/server/middleware"

	"github.com/Ignaciojeria/ioc"
	"github.com/go-fuego/fuego"
	"github.com/google/uuid"
)

var _ = ioc.Register(projectsHandler)

var projectNameRegex = regexp.MustCompile(`^[a-z0-9-]{3,40}$`)

type createProjectRequest struct {
	Name string `json:"name" validate:"required"`
}

type createProjectResponse struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Schema      string `json:"schema"`
	DatabaseURL string `json:"databaseUrl"`
}

func projectsHandler(s *server.Server, provisioner *postgresout.Provisioner) {
	fuego.Post(s.Server, "/projects", func(c fuego.ContextWithBody[createProjectRequest]) (createProjectResponse, error) {
		return createProject(c, provisioner)
	}, fuego.OptionDefaultStatusCode(http.StatusCreated))
}

func createProject(c fuego.ContextWithBody[createProjectRequest], provisioner *postgresout.Provisioner) (createProjectResponse, error) {
	claims, ok := middleware.JWTClaimsFromContext(c.Context())
	if !ok {
		return createProjectResponse{}, fuego.HTTPError{Status: http.StatusUnauthorized, Detail: "unauthorized"}
	}

	sub, ok := claims["sub"].(string)
	if !ok || strings.TrimSpace(sub) == "" {
		return createProjectResponse{}, fuego.HTTPError{Status: http.StatusUnauthorized, Detail: "missing sub claim"}
	}

	body, err := c.Body()
	if err != nil {
		return createProjectResponse{}, fuego.HTTPError{Status: http.StatusBadRequest, Detail: "invalid body"}
	}

	name := strings.TrimSpace(strings.ToLower(body.Name))
	if !projectNameRegex.MatchString(name) {
		return createProjectResponse{}, fuego.HTTPError{Status: http.StatusBadRequest, Detail: "name must match ^[a-z0-9-]{3,40}$"}
	}

	projectID := uuid.NewString()
	dbSafe := strings.ReplaceAll(name, "-", "_")
	dbName := normalizeIdentifier(dbSafe+"_db", 63)
	dbUser := normalizeIdentifier(dbSafe+"_user", 63)
	dbPassword, err := randomHex(24)
	if err != nil {
		return createProjectResponse{}, fuego.HTTPError{Status: http.StatusInternalServerError, Detail: "cannot generate password"}
	}

	ctx := c.Context()
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
	}

	result, err := provisioner.Provision(ctx, postgresout.ProvisionRequest{
		ID:       projectID,
		OwnerSub: sub,
		Name:     name,
		DBName:   dbName,
		DBUser:   dbUser,
		Password: dbPassword,
	})
	if err != nil {
		var conflict postgresout.ProjectConflictError
		if errors.As(err, &conflict) {
			return createProjectResponse{}, fuego.HTTPError{Status: http.StatusConflict, Detail: conflict.Detail}
		}
		return createProjectResponse{}, fuego.HTTPError{Status: http.StatusInternalServerError, Detail: "cannot provision database"}
	}

	return createProjectResponse{
		ID:          projectID,
		Name:        name,
		Schema:      dbName,
		DatabaseURL: result.DatabaseURL,
	}, nil
}

func randomHex(bytesLen int) (string, error) {
	b := make([]byte, bytesLen)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func normalizeIdentifier(base string, maxLen int) string {
	if len(base) <= maxLen {
		return base
	}
	id := uuid.NewString()
	hash := strings.ReplaceAll(id[:8], "-", "")
	prefixLen := maxLen - 1 - len(hash)
	if prefixLen < 1 {
		return hash[:maxLen]
	}
	return base[:prefixLen] + "_" + hash
}
