package in

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strings"
	"time"

	postgresout "postgresql/internal/adapter/out/postgres"
	"postgresql/internal/shared/server"
	"postgresql/internal/shared/server/middleware"

	"github.com/Ignaciojeria/ioc"
	"github.com/go-fuego/fuego"
)

var _ = ioc.Register(electricHandler)

const defaultElectricTarget = "http://127.0.0.1:13000"

var electricIdentifierRegex = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

func electricHandler(s *server.Server, provisioner *postgresout.Provisioner) {
	fuego.Get(s.Server, "/projects/{project}/sync", func(c fuego.ContextNoBody) (any, error) {
		return nil, handleElectricShape(c, provisioner)
	})
}

func handleElectricShape(c fuego.ContextNoBody, provisioner *postgresout.Provisioner) error {
	ctx := c.Context()
	claims, ok := middleware.JWTClaimsFromContext(ctx)
	if !ok {
		return fuego.HTTPError{Status: http.StatusUnauthorized, Detail: "unauthorized"}
	}

	sub, ok := claims["sub"].(string)
	if !ok || strings.TrimSpace(sub) == "" {
		return fuego.HTTPError{Status: http.StatusUnauthorized, Detail: "missing sub claim"}
	}

	projectIDOrName := c.PathParam("project")
	if projectIDOrName == "" {
		return fuego.HTTPError{Status: http.StatusBadRequest, Detail: "missing project"}
	}

	lookupCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	project, found, err := provisioner.FindProjectForOwner(lookupCtx, sub, projectIDOrName)
	if err != nil {
		slog.Error("cannot authorize electric sync", "err", err, "sub", sub, "project", projectIDOrName)
		return fuego.HTTPError{Status: http.StatusInternalServerError, Detail: "cannot authorize sync"}
	}
	if !found {
		return fuego.HTTPError{Status: http.StatusNotFound, Detail: "project not found"}
	}

	ensureCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	project, err = provisioner.EnsureElectric(ensureCtx, project)
	if err != nil {
		slog.Error("cannot ensure electric sync", "err", err, "project_id", project.ID)
		return fuego.HTTPError{Status: http.StatusBadGateway, Detail: "cannot provision sync"}
	}
	if project.ElectricPort == 0 || project.ElectricSecret == "" {
		return fuego.HTTPError{Status: http.StatusBadGateway, Detail: "sync unavailable"}
	}

	table := strings.TrimSpace(c.QueryParam("table"))
	if table == "" {
		return fuego.HTTPError{Status: http.StatusBadRequest, Detail: "missing table query parameter"}
	}
	tenantTable, err := tenantScopedElectricTable(project.DBSchema, table)
	if err != nil {
		return fuego.HTTPError{Status: http.StatusBadRequest, Detail: err.Error()}
	}

	proxy, err := newElectricProxy(project.ElectricURL(), project.ElectricSecret)
	if err != nil {
		slog.Error("cannot create electric proxy", "err", err, "project_id", project.ID)
		return fuego.HTTPError{Status: http.StatusBadGateway, Detail: "sync unavailable"}
	}

	req := c.Request()
	proxied := req.Clone(ctx)
	proxied.URL.Path = "/v1/shape"
	proxied.URL.RawPath = ""
	q := req.URL.Query()
	q.Set("table", tenantTable)
	proxied.URL.RawQuery = q.Encode()
	proxied.Header = req.Header.Clone()
	proxied.Header.Del("Authorization")
	proxied.Header.Del("Cookie")
	proxied.Header.Set("X-Forwarded-Project-ID", project.ID)
	proxied.Header.Set("X-Forwarded-Project", project.Name)
	proxied.Header.Set("X-Forwarded-User", sub)

	proxy.ServeHTTP(c.Response(), proxied)
	return nil
}

func tenantScopedElectricTable(schema, requested string) (string, error) {
	schema = strings.TrimSpace(schema)
	requested = strings.TrimSpace(requested)
	if !electricIdentifierRegex.MatchString(schema) {
		return "", errors.New("invalid project schema")
	}
	parts := strings.Split(requested, ".")
	if len(parts) == 1 {
		if !electricIdentifierRegex.MatchString(parts[0]) {
			return "", errors.New("invalid table query parameter")
		}
		return schema + "." + parts[0], nil
	}
	if len(parts) == 2 {
		if !electricIdentifierRegex.MatchString(parts[0]) || !electricIdentifierRegex.MatchString(parts[1]) {
			return "", errors.New("invalid table query parameter")
		}
		if parts[0] != schema {
			return "", errors.New("table is outside the project schema")
		}
		return requested, nil
	}
	return "", errors.New("invalid table query parameter")
}

func newElectricProxy(targetRaw string, secret string) (*httputil.ReverseProxy, error) {
	target, err := url.Parse(targetRaw)
	if err != nil {
		return nil, err
	}
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return nil, errors.New("electric secret is required")
	}

	proxy := httputil.NewSingleHostReverseProxy(target)
	originalDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		originalDirector(req)
		req.URL.Scheme = target.Scheme
		req.URL.Host = target.Host
		req.URL.Path = "/v1/shape"
		req.Host = target.Host

		q := req.URL.Query()
		q.Set("secret", secret)
		req.URL.RawQuery = q.Encode()
	}
	proxy.Transport = &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		slog.Error("electric proxy error", "err", err)
		http.Error(w, "electric unavailable", http.StatusBadGateway)
	}
	return proxy, nil
}
