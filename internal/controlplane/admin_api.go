package controlplane

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"

	"github.com/darkinno/edge-dispatch-framework/internal/auth"
	"github.com/darkinno/edge-dispatch-framework/internal/config"
	"github.com/darkinno/edge-dispatch-framework/internal/models"
	"github.com/darkinno/edge-dispatch-framework/internal/store"
)

// AdminAPI handles all admin console HTTP endpoints.
type AdminAPI struct {
	pg         *store.PGStore
	redis      *store.RedisStore
	registry   *Registry
	cfg        *config.AdminAPIConfig
	adminAuth  *auth.AdminAuth
	nonceCache *auth.NonceCache
	jwt        *auth.JWTSession
}

// NewAdminAPI creates a new admin API handler group.
func NewAdminAPI(pg *store.PGStore, redis *store.RedisStore, registry *Registry, adminCfg *config.AdminAPIConfig) (http.Handler, error) {
	if adminCfg == nil || !adminCfg.Enabled {
		return nil, nil
	}

	adminAuth := auth.NewAdminAuth(adminCfg.AdminAccessKey, adminCfg.AdminSecretKey, 5*time.Minute, 30*time.Second)
	nonceCache := auth.NewNonceCache(5 * time.Minute)
	jwt := auth.NewJWTSession(adminCfg.JWTSecret, adminCfg.JWTExpirySeconds)

	a := &AdminAPI{
		pg:         pg,
		redis:      redis,
		registry:   registry,
		cfg:        adminCfg,
		adminAuth:  adminAuth,
		nonceCache: nonceCache,
		jwt:        jwt,
	}

	// Seed default tenant and admin user if not exist
	ctx := context.Background()
	if _, err := pg.GetTenant(ctx, "default"); err != nil {
		defaultTenant := &models.Tenant{Name: "Default", Description: "Default tenant"}
		if err := pg.CreateTenant(ctx, defaultTenant); err != nil {
			slog.Warn("failed to seed default tenant", "err", err)
		}
		// Seed default admin user
		seedPassword := "admin123"
		if envSeed := os.Getenv("CP_ADMIN_SEED_PASSWORD"); envSeed != "" {
			seedPassword = envSeed
		} else {
			slog.Warn("CP_ADMIN_SEED_PASSWORD not set, using default seed password for initial admin user")
		}
		hash, _ := bcrypt.GenerateFromPassword([]byte(seedPassword), bcrypt.DefaultCost)
		adminUser := &models.User{
			TenantID:    "default",
			Email:       "admin@edf.local",
			DisplayName: "Admin",
			Roles: []models.RoleBinding{
				{Role: models.UserRoleTenantOwner, TenantID: "default"},
			},
		}
		if err := pg.CreateUser(ctx, adminUser, string(hash)); err != nil {
			slog.Warn("failed to seed admin user", "err", err)
		}
	}

	r := chi.NewRouter()

	// Health check for admin API
	r.Get("/healthz", a.handleAdminHealthz)

	// Auth routes (no JWT required)
	r.Post("/login", a.handleLogin)
	r.Post("/refresh", a.handleRefresh)

	// Protected routes (JWT required)
	r.Group(func(r chi.Router) {
		r.Use(jwt.Middleware)

		r.Post("/logout", a.handleLogout)
		r.Get("/me", a.handleMe)

		// Tenants
		r.Post("/tenants", a.handleCreateTenant)
		r.Get("/tenants", a.handleListTenants)
		r.Get("/tenants/{tenantID}", a.handleGetTenant)
		r.Put("/tenants/{tenantID}", a.handleUpdateTenant)
		r.Delete("/tenants/{tenantID}", a.handleDeleteTenant)
		r.Get("/tenants/{tenantID}/users", a.handleListUsers)

		// Projects
		r.Post("/tenants/{tenantID}/projects", a.handleCreateProject)
		r.Get("/tenants/{tenantID}/projects", a.handleListProjects)
		r.Get("/projects/{projectID}", a.handleGetProject)
		r.Put("/projects/{projectID}", a.handleUpdateProject)
		r.Delete("/projects/{projectID}", a.handleDeleteProject)

		// Users
		r.Post("/users", a.handleCreateUser)
		r.Get("/users/{userID}", a.handleGetUser)
		r.Put("/users/{userID}", a.handleUpdateUser)
		r.Delete("/users/{userID}", a.handleDeleteUser)

		// Node management
		r.Get("/nodes", a.handleListNodes)
		r.Get("/nodes/{nodeID}", a.handleGetNode)
		r.Post("/nodes/{nodeID}:disable", a.handleDisableNode)
		r.Post("/nodes/{nodeID}:enable", a.handleEnableNode)
		r.Post("/nodes/{nodeID}:revoke", a.handleRevokeNode)
		r.Patch("/nodes/{nodeID}", a.handlePatchNode)

		// Policy management
		r.Post("/policies", a.handleCreatePolicy)
		r.Get("/policies", a.handleListPolicies)
		r.Get("/policies/{policyID}", a.handleGetPolicy)
		r.Put("/policies/{policyID}", a.handleUpdatePolicy)
		r.Delete("/policies/{policyID}", a.handleDeletePolicy)
		r.Post("/policies/{policyID}:publish", a.handlePublishPolicy)
		r.Post("/policies/{policyID}:rollback", a.handleRollbackPolicy)
		r.Get("/policies/{policyID}/versions", a.handleGetPolicyVersions)

		// Ingress management
		r.Post("/ingresses", a.handleCreateIngress)
		r.Get("/ingresses", a.handleListIngresses)
		r.Get("/ingresses/{ingressID}", a.handleGetIngress)
		r.Put("/ingresses/{ingressID}", a.handleUpdateIngress)
		r.Delete("/ingresses/{ingressID}", a.handleDeleteIngress)

		// Cache operations
		r.Post("/cache/prewarm", a.handlePrewarm)
		r.Post("/cache/purge", a.handlePurge)
		r.Post("/objects/block", a.handleBlock)

		// Task management
		r.Get("/tasks", a.handleListTasks)
		r.Get("/tasks/{taskID}", a.handleGetTask)
		r.Post("/tasks/{taskID}:cancel", a.handleCancelTask)

		// Audit
		r.Get("/audit", a.handleQueryAudit)
		r.Get("/audit/export", a.handleExportAudit)

		// Dashboard
		r.Get("/dashboard", a.handleDashboard)

		// Settings
		r.Get("/settings", a.handleGetSettings)
		r.Put("/settings", a.handleUpdateSettings)
	})

	return r, nil
}

// ─── Auth Handlers ─────────────────────────────────────────────

var adminLoginLimiter = newRateLimiter(5, 10)

func (a *AdminAPI) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !adminLoginLimiter.Allow() {
		a.writeError(w, http.StatusTooManyRequests, "RATE_LIMITED", "too many login attempts, try again later")
		return
	}

	if !a.cfg.EnableLocalAuth {
		a.writeError(w, http.StatusForbidden, "LOCAL_AUTH_DISABLED", "local authentication is disabled")
		return
	}

	if ct := r.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(ct, "application/json") {
		a.writeError(w, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE", "Content-Type must be application/json")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBodySize)

	var req models.LoginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		a.writeError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid request body")
		return
	}

	user, passwordHash, err := a.pg.GetUserByEmail(r.Context(), req.Email)
	if err != nil {
		a.writeError(w, http.StatusUnauthorized, "INVALID_CREDENTIALS", "invalid email or password")
		return
	}

	if bcrypt.CompareHashAndPassword([]byte(passwordHash), []byte(req.Password)) != nil {
		a.writeError(w, http.StatusUnauthorized, "INVALID_CREDENTIALS", "invalid email or password")
		return
	}

	token, exp, err := a.jwt.Sign(user.UserID, user.TenantID, user.Email)
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "AUTH_FAILED", "failed to sign token")
		return
	}

	refreshToken := auth.GenerateRefreshToken()
	// Store refresh token hash (SHA-256) in DB for lookup
	refreshHash := sha256Hash(refreshToken)
	if err := a.pg.UpdateUserRefreshToken(r.Context(), user.UserID, refreshHash); err != nil {
		slog.Warn("failed to store refresh token", "user_id", user.UserID, "err", err)
	}

	a.audit(r, models.AuditActionLogin, "user", user.UserID, "", "success")

	a.writeJSON(w, http.StatusOK, models.LoginResponse{
		Token:        token,
		RefreshToken: refreshToken,
		User:         *user,
		ExpiresAt:    exp,
		Roles:        user.Roles,
	})
}

func (a *AdminAPI) handleRefresh(w http.ResponseWriter, r *http.Request) {
	if ct := r.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(ct, "application/json") {
		a.writeError(w, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE", "Content-Type must be application/json")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBodySize)
	var body struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		a.writeError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid request body")
		return
	}
	if body.RefreshToken == "" {
		a.writeError(w, http.StatusBadRequest, "BAD_REQUEST", "refresh_token is required")
		return
	}

	// Look up user by refresh token hash
	refreshHash := sha256Hash(body.RefreshToken)
	userID, err := a.pg.GetUserIDByRefreshToken(r.Context(), refreshHash)
	if err != nil {
		a.writeError(w, http.StatusUnauthorized, "INVALID_REFRESH_TOKEN", "invalid refresh token")
		return
	}

	user, err := a.pg.GetUser(r.Context(), userID)
	if err != nil {
		a.writeError(w, http.StatusUnauthorized, "INVALID_REFRESH_TOKEN", "invalid refresh token")
		return
	}

	token, exp, err := a.jwt.Sign(user.UserID, user.TenantID, user.Email)
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "AUTH_FAILED", "failed to refresh token")
		return
	}

	// Rotate refresh token
	newRefreshToken := auth.GenerateRefreshToken()
	newHash := sha256Hash(newRefreshToken)
	if err := a.pg.UpdateUserRefreshToken(r.Context(), user.UserID, newHash); err != nil {
		slog.Warn("failed to rotate refresh token", "user_id", user.UserID, "err", err)
	}

	a.writeJSON(w, http.StatusOK, map[string]interface{}{
		"token":         token,
		"expires_at":    exp,
		"refresh_token": newRefreshToken,
	})
}

func (a *AdminAPI) handleLogout(w http.ResponseWriter, r *http.Request) {
	actorID := r.Header.Get("X-Actor-UserId")
	// Clear refresh token on logout
	if err := a.pg.UpdateUserRefreshToken(r.Context(), actorID, ""); err != nil {
		slog.Warn("failed to clear refresh token on logout", "user_id", actorID, "err", err)
	}
	a.audit(r, models.AuditActionLogout, "user", actorID, "", "success")
	a.writeJSON(w, http.StatusOK, map[string]string{"status": "logged_out"})
}

func (a *AdminAPI) handleMe(w http.ResponseWriter, r *http.Request) {
	userID := r.Header.Get("X-Actor-UserId")
	user, err := a.pg.GetUser(r.Context(), userID)
	if err != nil {
		a.writeError(w, http.StatusNotFound, "NOT_FOUND", "user not found")
		return
	}
	a.writeJSON(w, http.StatusOK, user)
}

func (a *AdminAPI) handleAdminHealthz(w http.ResponseWriter, r *http.Request) {
	a.writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "time": time.Now().UTC().Format(time.RFC3339)})
}

// ─── Tenant Handlers ───────────────────────────────────────────

func (a *AdminAPI) handleCreateTenant(w http.ResponseWriter, r *http.Request) {
	if ct := r.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(ct, "application/json") {
		a.writeError(w, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE", "Content-Type must be application/json")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBodySize)
	var tenant models.Tenant
	if err := json.NewDecoder(r.Body).Decode(&tenant); err != nil {
		a.writeError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid request body")
		return
	}
	tenant.Name = strings.TrimSpace(tenant.Name)
	if tenant.Name == "" {
		a.writeError(w, http.StatusBadRequest, "BAD_REQUEST", "name is required")
		return
	}
	if err := a.pg.CreateTenant(r.Context(), &tenant); err != nil {
		a.writeError(w, http.StatusInternalServerError, "CREATE_FAILED", "failed to create tenant")
		return
	}
	a.audit(r, models.AuditActionPolicyCreate, "tenant", tenant.TenantID, "", "success")
	a.writeJSON(w, http.StatusCreated, tenant)
}

func (a *AdminAPI) handleListTenants(w http.ResponseWriter, r *http.Request) {
	tenants, err := a.pg.ListTenants(r.Context())
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "LIST_FAILED", "failed to list tenants")
		return
	}
	if tenants == nil {
		tenants = []*models.Tenant{}
	}
	a.writeJSON(w, http.StatusOK, tenants)
}

func (a *AdminAPI) handleGetTenant(w http.ResponseWriter, r *http.Request) {
	tenantID := chi.URLParam(r, "tenantID")
	tenant, err := a.pg.GetTenant(r.Context(), tenantID)
	if err != nil {
		a.writeError(w, http.StatusNotFound, "NOT_FOUND", "tenant not found")
		return
	}
	a.writeJSON(w, http.StatusOK, tenant)
}

func (a *AdminAPI) handleUpdateTenant(w http.ResponseWriter, r *http.Request) {
	if ct := r.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(ct, "application/json") {
		a.writeError(w, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE", "Content-Type must be application/json")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBodySize)
	tenantID := chi.URLParam(r, "tenantID")
	existing, err := a.pg.GetTenant(r.Context(), tenantID)
	if err != nil {
		a.writeError(w, http.StatusNotFound, "NOT_FOUND", "tenant not found")
		return
	}
	var update models.Tenant
	if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
		a.writeError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid request body")
		return
	}
	if update.Name != "" {
		existing.Name = update.Name
	}
	if update.Description != "" {
		existing.Description = update.Description
	}
	if err := a.pg.UpdateTenant(r.Context(), existing); err != nil {
		a.writeError(w, http.StatusInternalServerError, "UPDATE_FAILED", "failed to update tenant")
		return
	}
	a.audit(r, models.AuditActionPolicyUpdate, "tenant", tenantID, "", "success")
	a.writeJSON(w, http.StatusOK, existing)
}

func (a *AdminAPI) handleDeleteTenant(w http.ResponseWriter, r *http.Request) {
	tenantID := chi.URLParam(r, "tenantID")
	if err := a.pg.DeleteTenant(r.Context(), tenantID); err != nil {
		a.writeError(w, http.StatusInternalServerError, "DELETE_FAILED", "failed to delete tenant")
		return
	}
	a.audit(r, models.AuditActionPolicyUpdate, "tenant", tenantID, "", "success")
	a.writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (a *AdminAPI) handleListUsers(w http.ResponseWriter, r *http.Request) {
	tenantID := chi.URLParam(r, "tenantID")
	users, err := a.pg.ListUsers(r.Context(), tenantID)
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "LIST_FAILED", "failed to list users")
		return
	}
	if users == nil {
		users = []*models.User{}
	}
	a.writeJSON(w, http.StatusOK, users)
}

// ─── Project Handlers ──────────────────────────────────────────

func (a *AdminAPI) handleCreateProject(w http.ResponseWriter, r *http.Request) {
	if ct := r.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(ct, "application/json") {
		a.writeError(w, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE", "Content-Type must be application/json")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBodySize)
	tenantID := chi.URLParam(r, "tenantID")
	var project models.Project
	if err := json.NewDecoder(r.Body).Decode(&project); err != nil {
		a.writeError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid request body")
		return
	}
	project.TenantID = tenantID
	project.Name = strings.TrimSpace(project.Name)
	if project.Name == "" {
		a.writeError(w, http.StatusBadRequest, "BAD_REQUEST", "name is required")
		return
	}
	if err := a.pg.CreateProject(r.Context(), &project); err != nil {
		a.writeError(w, http.StatusInternalServerError, "CREATE_FAILED", "failed to create project")
		return
	}
	a.audit(r, models.AuditActionPolicyCreate, "project", project.ProjectID, "", "success")
	a.writeJSON(w, http.StatusCreated, project)
}

func (a *AdminAPI) handleListProjects(w http.ResponseWriter, r *http.Request) {
	tenantID := chi.URLParam(r, "tenantID")
	projects, err := a.pg.ListProjects(r.Context(), tenantID)
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "LIST_FAILED", "failed to list projects")
		return
	}
	if projects == nil {
		projects = []*models.Project{}
	}
	a.writeJSON(w, http.StatusOK, projects)
}

func (a *AdminAPI) handleGetProject(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	project, err := a.pg.GetProject(r.Context(), projectID)
	if err != nil {
		a.writeError(w, http.StatusNotFound, "NOT_FOUND", "project not found")
		return
	}
	a.writeJSON(w, http.StatusOK, project)
}

func (a *AdminAPI) handleUpdateProject(w http.ResponseWriter, r *http.Request) {
	if ct := r.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(ct, "application/json") {
		a.writeError(w, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE", "Content-Type must be application/json")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBodySize)
	projectID := chi.URLParam(r, "projectID")
	existing, err := a.pg.GetProject(r.Context(), projectID)
	if err != nil {
		a.writeError(w, http.StatusNotFound, "NOT_FOUND", "project not found")
		return
	}
	var update models.Project
	if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
		a.writeError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid request body")
		return
	}
	if update.Name != "" {
		existing.Name = update.Name
	}
	if update.Description != "" {
		existing.Description = update.Description
	}
	if err := a.pg.UpdateProject(r.Context(), existing); err != nil {
		a.writeError(w, http.StatusInternalServerError, "UPDATE_FAILED", "failed to update project")
		return
	}
	a.audit(r, models.AuditActionPolicyUpdate, "project", projectID, "", "success")
	a.writeJSON(w, http.StatusOK, existing)
}

func (a *AdminAPI) handleDeleteProject(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	if err := a.pg.DeleteProject(r.Context(), projectID); err != nil {
		a.writeError(w, http.StatusInternalServerError, "DELETE_FAILED", "failed to delete project")
		return
	}
	a.audit(r, models.AuditActionPolicyUpdate, "project", projectID, "", "success")
	a.writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// ─── User Handlers ─────────────────────────────────────────────

func (a *AdminAPI) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	if ct := r.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(ct, "application/json") {
		a.writeError(w, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE", "Content-Type must be application/json")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBodySize)
	var body struct {
		TenantID    string              `json:"tenant_id"`
		Email       string              `json:"email"`
		DisplayName string              `json:"display_name"`
		Password    string              `json:"password"`
		Roles       []models.RoleBinding `json:"roles"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		a.writeError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid request body")
		return
	}
	body.Email = strings.TrimSpace(body.Email)
	if body.Email == "" || body.Password == "" {
		a.writeError(w, http.StatusBadRequest, "BAD_REQUEST", "email and password are required")
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(body.Password), bcrypt.DefaultCost)
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "CREATE_FAILED", "failed to hash password")
		return
	}
	user := &models.User{
		TenantID:    body.TenantID,
		Email:       body.Email,
		DisplayName: body.DisplayName,
		Roles:       body.Roles,
	}
	if err := a.pg.CreateUser(r.Context(), user, string(hash)); err != nil {
		a.writeError(w, http.StatusInternalServerError, "CREATE_FAILED", "failed to create user")
		return
	}
	a.audit(r, models.AuditActionUserCreate, "user", user.UserID, "", "success")
	a.writeJSON(w, http.StatusCreated, user)
}

func (a *AdminAPI) handleGetUser(w http.ResponseWriter, r *http.Request) {
	userID := chi.URLParam(r, "userID")
	user, err := a.pg.GetUser(r.Context(), userID)
	if err != nil {
		a.writeError(w, http.StatusNotFound, "NOT_FOUND", "user not found")
		return
	}
	a.writeJSON(w, http.StatusOK, user)
}

func (a *AdminAPI) handleUpdateUser(w http.ResponseWriter, r *http.Request) {
	if ct := r.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(ct, "application/json") {
		a.writeError(w, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE", "Content-Type must be application/json")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBodySize)
	userID := chi.URLParam(r, "userID")
	existing, err := a.pg.GetUser(r.Context(), userID)
	if err != nil {
		a.writeError(w, http.StatusNotFound, "NOT_FOUND", "user not found")
		return
	}
	var update struct {
		DisplayName string              `json:"display_name"`
		Roles       []models.RoleBinding `json:"roles"`
	}
	if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
		a.writeError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid request body")
		return
	}
	if update.DisplayName != "" {
		existing.DisplayName = update.DisplayName
	}
	if update.Roles != nil {
		existing.Roles = update.Roles
	}
	if err := a.pg.UpdateUser(r.Context(), existing); err != nil {
		a.writeError(w, http.StatusInternalServerError, "UPDATE_FAILED", "failed to update user")
		return
	}
	a.audit(r, models.AuditActionUserUpdate, "user", userID, "", "success")
	a.writeJSON(w, http.StatusOK, existing)
}

func (a *AdminAPI) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	userID := chi.URLParam(r, "userID")
	if err := a.pg.DeleteUser(r.Context(), userID); err != nil {
		a.writeError(w, http.StatusInternalServerError, "DELETE_FAILED", "failed to delete user")
		return
	}
	a.audit(r, models.AuditActionUserUpdate, "user", userID, "", "success")
	a.writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// ─── Node Management Handlers ──────────────────────────────────

func (a *AdminAPI) handleListNodes(w http.ResponseWriter, r *http.Request) {
	tenantID := r.URL.Query().Get("tenant_id")
	projectID := r.URL.Query().Get("project_id")
	statusFilter := r.URL.Query().Get("status")
	regionFilter := r.URL.Query().Get("region")
	ispFilter := r.URL.Query().Get("isp")
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if limit <= 0 {
		limit = 50
	}

	nodes, total, err := a.pg.ListNodes(r.Context(), tenantID, statusFilter, regionFilter, ispFilter, projectID, limit, offset)
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "LIST_FAILED", "failed to list nodes")
		return
	}
	if nodes == nil {
		nodes = []*models.Node{}
	}
	a.writeJSON(w, http.StatusOK, models.PaginatedResponse{
		Data:    nodes,
		Total:   total,
		Limit:   limit,
		Offset:  offset,
		HasMore: offset+limit < total,
	})
}

func (a *AdminAPI) handleGetNode(w http.ResponseWriter, r *http.Request) {
	nodeID := chi.URLParam(r, "nodeID")
	node, err := a.registry.GetNode(r.Context(), nodeID)
	if err != nil {
		a.writeError(w, http.StatusNotFound, "NOT_FOUND", "node not found")
		return
	}
	a.writeJSON(w, http.StatusOK, node)
}

func (a *AdminAPI) handleDisableNode(w http.ResponseWriter, r *http.Request) {
	nodeID := chi.URLParam(r, "nodeID")

	if ct := r.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(ct, "application/json") {
		a.writeError(w, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE", "Content-Type must be application/json")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBodySize)

	var body struct {
		Reason  string `json:"reason"`
		Message string `json:"message"`
		Until   string `json:"until"` // RFC3339 timestamp
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		a.writeError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid request body")
		return
	}

	var until time.Time
	if body.Until != "" {
		until, _ = time.Parse(time.RFC3339, body.Until)
	}

	if err := a.pg.NodeDisable(r.Context(), nodeID, body.Reason, body.Message, until); err != nil {
		a.writeError(w, http.StatusInternalServerError, "DISABLE_FAILED", "failed to disable node")
		return
	}
	a.audit(r, models.AuditActionNodeDisable, "node", nodeID, "", "success")
	a.writeJSON(w, http.StatusOK, map[string]string{"status": "disabled", "node_id": nodeID})
}

func (a *AdminAPI) handleEnableNode(w http.ResponseWriter, r *http.Request) {
	nodeID := chi.URLParam(r, "nodeID")
	if err := a.pg.NodeEnable(r.Context(), nodeID); err != nil {
		a.writeError(w, http.StatusInternalServerError, "ENABLE_FAILED", "failed to enable node")
		return
	}
	a.audit(r, models.AuditActionNodeEnable, "node", nodeID, "", "success")
	a.writeJSON(w, http.StatusOK, map[string]string{"status": "enabled", "node_id": nodeID})
}

func (a *AdminAPI) handleRevokeNode(w http.ResponseWriter, r *http.Request) {
	nodeID := chi.URLParam(r, "nodeID")
	if err := a.registry.RevokeNode(r.Context(), nodeID); err != nil {
		a.writeError(w, http.StatusInternalServerError, "REVOKE_FAILED", "failed to revoke node")
		return
	}
	// Mark node as revoked in Redis so heartbeats are rejected
	if err := a.redis.MarkNodeRevoked(r.Context(), nodeID); err != nil {
		slog.Warn("failed to mark node revoked in redis", "node_id", nodeID, "err", err)
	}
	a.audit(r, models.AuditActionNodeRevoke, "node", nodeID, "", "success")
	a.writeJSON(w, http.StatusOK, map[string]string{"status": "revoked", "node_id": nodeID})
}

func (a *AdminAPI) handlePatchNode(w http.ResponseWriter, r *http.Request) {
	if ct := r.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(ct, "application/json") {
		a.writeError(w, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE", "Content-Type must be application/json")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBodySize)
	nodeID := chi.URLParam(r, "nodeID")
	var patch models.NodeAdminPatch
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		a.writeError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid request body")
		return
	}

	// Handle disable/enable
	if patch.Disabled != nil && *patch.Disabled {
		reason := ""
		if patch.DisableReason != nil {
			reason = *patch.DisableReason
		}
		var until time.Time
		if patch.DisableUntil != nil {
			until = *patch.DisableUntil
		}
		if err := a.pg.NodeDisable(r.Context(), nodeID, reason, "", until); err != nil {
			a.writeError(w, http.StatusInternalServerError, "PATCH_FAILED", "failed to patch node")
			return
		}
	} else if patch.Disabled != nil && !*patch.Disabled {
		if err := a.pg.NodeEnable(r.Context(), nodeID); err != nil {
			a.writeError(w, http.StatusInternalServerError, "PATCH_FAILED", "failed to patch node")
			return
		}
		// Also unrevoke from Redis if previously revoked
		a.redis.UnrevokeNode(r.Context(), nodeID)
	}

	// Handle patching labels, weight, project_id, tenant_id
	updateQuery := `UPDATE nodes SET updated_at=NOW()`
	updateArgs := []interface{}{}
	argIdx := 1
	if patch.Labels != nil {
		labelsJSON, _ := json.Marshal(patch.Labels)
		updateQuery += fmt.Sprintf(", labels=$%d", argIdx)
		updateArgs = append(updateArgs, labelsJSON)
		argIdx++
	}
	if patch.Weight != nil {
		updateQuery += fmt.Sprintf(", weight=$%d", argIdx)
		updateArgs = append(updateArgs, *patch.Weight)
		argIdx++
	}
	if patch.ProjectID != nil {
		updateQuery += fmt.Sprintf(", project_id=$%d", argIdx)
		updateArgs = append(updateArgs, *patch.ProjectID)
		argIdx++
	}
	if patch.TenantID != nil {
		updateQuery += fmt.Sprintf(", tenant_id=$%d", argIdx)
		updateArgs = append(updateArgs, *patch.TenantID)
		argIdx++
	}

	if len(updateArgs) > 0 {
		updateQuery += fmt.Sprintf(" WHERE node_id=$%d", argIdx)
		updateArgs = append(updateArgs, nodeID)
		if _, err := a.pg.Pool().Exec(r.Context(), updateQuery, updateArgs...); err != nil {
			slog.Warn("patch node failed", "node_id", nodeID, "err", err)
		} else {
			a.pg.InvalidateNodeCache(nodeID)
		}
	}

	a.audit(r, models.AuditActionNodePatch, "node", nodeID, "", "success")
	a.writeJSON(w, http.StatusOK, map[string]string{"status": "patched", "node_id": nodeID})
}

// ─── Policy Handlers ───────────────────────────────────────────

func (a *AdminAPI) handleCreatePolicy(w http.ResponseWriter, r *http.Request) {
	if ct := r.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(ct, "application/json") {
		a.writeError(w, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE", "Content-Type must be application/json")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBodySize)
	var policy models.AdminPolicy
	if err := json.NewDecoder(r.Body).Decode(&policy); err != nil {
		a.writeError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid request body")
		return
	}
	policy.Name = strings.TrimSpace(policy.Name)
	if policy.Name == "" {
		a.writeError(w, http.StatusBadRequest, "BAD_REQUEST", "policy name is required")
		return
	}
	if err := a.pg.CreateAdminPolicy(r.Context(), &policy); err != nil {
		a.writeError(w, http.StatusInternalServerError, "CREATE_FAILED", "failed to create policy")
		return
	}
	// Save initial version
	pv := &models.AdminPolicyVersion{
		PolicyID: policy.PolicyID,
		Version:  1,
		Content:  policy.Content,
	}
	a.pg.SavePolicyVersion(r.Context(), pv)

	a.audit(r, models.AuditActionPolicyCreate, "policy", policy.PolicyID, "", "success")
	a.writeJSON(w, http.StatusCreated, policy)
}

func (a *AdminAPI) handleListPolicies(w http.ResponseWriter, r *http.Request) {
	tenantID := r.URL.Query().Get("tenant_id")
	projectID := r.URL.Query().Get("project_id")
	policies, err := a.pg.ListAdminPolicies(r.Context(), tenantID, projectID)
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "LIST_FAILED", "failed to list policies")
		return
	}
	if policies == nil {
		policies = []*models.AdminPolicy{}
	}
	a.writeJSON(w, http.StatusOK, policies)
}

func (a *AdminAPI) handleGetPolicy(w http.ResponseWriter, r *http.Request) {
	policyID := chi.URLParam(r, "policyID")
	policy, err := a.pg.GetAdminPolicy(r.Context(), policyID)
	if err != nil {
		a.writeError(w, http.StatusNotFound, "NOT_FOUND", "policy not found")
		return
	}
	a.writeJSON(w, http.StatusOK, policy)
}

func (a *AdminAPI) handleUpdatePolicy(w http.ResponseWriter, r *http.Request) {
	if ct := r.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(ct, "application/json") {
		a.writeError(w, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE", "Content-Type must be application/json")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBodySize)
	policyID := chi.URLParam(r, "policyID")
	existing, err := a.pg.GetAdminPolicy(r.Context(), policyID)
	if err != nil {
		a.writeError(w, http.StatusNotFound, "NOT_FOUND", "policy not found")
		return
	}
	var update models.AdminPolicy
	if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
		a.writeError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid request body")
		return
	}
	if update.Name != "" {
		existing.Name = update.Name
	}
	if update.Type != "" {
		existing.Type = update.Type
	}
	if update.Content != nil {
		existing.Content = update.Content
	}
	if update.Description != "" {
		existing.Description = update.Description
	}
	if err := a.pg.UpdateAdminPolicy(r.Context(), existing); err != nil {
		a.writeError(w, http.StatusInternalServerError, "UPDATE_FAILED", "failed to update policy")
		return
	}
	// Re-get to have updated version number
	updated, _ := a.pg.GetAdminPolicy(r.Context(), policyID)
	if updated != nil {
		pv := &models.AdminPolicyVersion{
			PolicyID: updated.PolicyID,
			Version:  updated.Version,
			Content:  updated.Content,
		}
		a.pg.SavePolicyVersion(r.Context(), pv)
	}
	a.audit(r, models.AuditActionPolicyUpdate, "policy", policyID, "", "success")
	a.writeJSON(w, http.StatusOK, existing)
}

func (a *AdminAPI) handleDeletePolicy(w http.ResponseWriter, r *http.Request) {
	policyID := chi.URLParam(r, "policyID")
	if err := a.pg.DeleteAdminPolicy(r.Context(), policyID); err != nil {
		a.writeError(w, http.StatusInternalServerError, "DELETE_FAILED", "failed to delete policy")
		return
	}
	a.audit(r, models.AuditActionPolicyUpdate, "policy", policyID, "", "success")
	a.writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (a *AdminAPI) handlePublishPolicy(w http.ResponseWriter, r *http.Request) {
	policyID := chi.URLParam(r, "policyID")
	if err := a.pg.PublishAdminPolicy(r.Context(), policyID); err != nil {
		a.writeError(w, http.StatusInternalServerError, "PUBLISH_FAILED", "failed to publish policy")
		return
	}
	a.audit(r, models.AuditActionPolicyPublish, "policy", policyID, "", "success")
	a.writeJSON(w, http.StatusOK, map[string]string{"status": "published", "policy_id": policyID})
}

func (a *AdminAPI) handleRollbackPolicy(w http.ResponseWriter, r *http.Request) {
	policyID := chi.URLParam(r, "policyID")
	versionStr := r.URL.Query().Get("to")
	if versionStr == "" {
		a.writeError(w, http.StatusBadRequest, "BAD_REQUEST", "version parameter required")
		return
	}
	version, _ := strconv.Atoi(versionStr)
	versions, err := a.pg.GetPolicyVersions(r.Context(), policyID)
	if err != nil || len(versions) == 0 {
		a.writeError(w, http.StatusNotFound, "NOT_FOUND", "version not found")
		return
	}
	var targetVersion *models.AdminPolicyVersion
	for _, v := range versions {
		if v.Version == version {
			targetVersion = v
			break
		}
	}
	if targetVersion == nil {
		a.writeError(w, http.StatusNotFound, "NOT_FOUND", "version not found")
		return
	}
	if err := a.pg.RollbackPolicy(r.Context(), policyID, targetVersion.Version, targetVersion.Content); err != nil {
		a.writeError(w, http.StatusInternalServerError, "ROLLBACK_FAILED", "failed to rollback policy")
		return
	}
	a.audit(r, models.AuditActionPolicyRollback, "policy", policyID, "", "success")
	a.writeJSON(w, http.StatusOK, map[string]string{"status": "rolled_back", "version": versionStr})
}

func (a *AdminAPI) handleGetPolicyVersions(w http.ResponseWriter, r *http.Request) {
	policyID := chi.URLParam(r, "policyID")
	versions, err := a.pg.GetPolicyVersions(r.Context(), policyID)
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "LIST_FAILED", "failed to list versions")
		return
	}
	if versions == nil {
		versions = []*models.AdminPolicyVersion{}
	}
	a.writeJSON(w, http.StatusOK, versions)
}

// ─── Ingress Handlers ──────────────────────────────────────────

func (a *AdminAPI) handleCreateIngress(w http.ResponseWriter, r *http.Request) {
	if ct := r.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(ct, "application/json") {
		a.writeError(w, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE", "Content-Type must be application/json")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBodySize)
	var ingress models.Ingress
	if err := json.NewDecoder(r.Body).Decode(&ingress); err != nil {
		a.writeError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid request body")
		return
	}
	ingress.Name = strings.TrimSpace(ingress.Name)
	if ingress.Name == "" {
		a.writeError(w, http.StatusBadRequest, "BAD_REQUEST", "ingress name is required")
		return
	}
	if err := a.pg.CreateIngress(r.Context(), &ingress); err != nil {
		a.writeError(w, http.StatusInternalServerError, "CREATE_FAILED", "failed to create ingress")
		return
	}
	a.audit(r, models.AuditActionIngressCreate, "ingress", ingress.IngressID, "", "success")
	a.writeJSON(w, http.StatusCreated, ingress)
}

func (a *AdminAPI) handleListIngresses(w http.ResponseWriter, r *http.Request) {
	tenantID := r.URL.Query().Get("tenant_id")
	projectID := r.URL.Query().Get("project_id")
	ingresses, err := a.pg.ListIngresses(r.Context(), tenantID, projectID)
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "LIST_FAILED", "failed to list ingresses")
		return
	}
	if ingresses == nil {
		ingresses = []*models.Ingress{}
	}
	a.writeJSON(w, http.StatusOK, ingresses)
}

func (a *AdminAPI) handleGetIngress(w http.ResponseWriter, r *http.Request) {
	ingressID := chi.URLParam(r, "ingressID")
	ingress, err := a.pg.GetIngress(r.Context(), ingressID)
	if err != nil {
		a.writeError(w, http.StatusNotFound, "NOT_FOUND", "ingress not found")
		return
	}
	a.writeJSON(w, http.StatusOK, ingress)
}

func (a *AdminAPI) handleUpdateIngress(w http.ResponseWriter, r *http.Request) {
	if ct := r.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(ct, "application/json") {
		a.writeError(w, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE", "Content-Type must be application/json")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBodySize)
	ingressID := chi.URLParam(r, "ingressID")
	existing, err := a.pg.GetIngress(r.Context(), ingressID)
	if err != nil {
		a.writeError(w, http.StatusNotFound, "NOT_FOUND", "ingress not found")
		return
	}
	var update models.Ingress
	if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
		a.writeError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid request body")
		return
	}
	if update.Name != "" {
		existing.Name = update.Name
	}
	if update.Type != "" {
		existing.Type = update.Type
	}
	if update.Domain != "" {
		existing.Domain = update.Domain
	}
	if update.Config != nil {
		existing.Config = update.Config
	}
	if err := a.pg.UpdateIngress(r.Context(), existing); err != nil {
		a.writeError(w, http.StatusInternalServerError, "UPDATE_FAILED", "failed to update ingress")
		return
	}
	a.audit(r, models.AuditActionIngressUpdate, "ingress", ingressID, "", "success")
	a.writeJSON(w, http.StatusOK, existing)
}

func (a *AdminAPI) handleDeleteIngress(w http.ResponseWriter, r *http.Request) {
	ingressID := chi.URLParam(r, "ingressID")
	if err := a.pg.DeleteIngress(r.Context(), ingressID); err != nil {
		a.writeError(w, http.StatusInternalServerError, "DELETE_FAILED", "failed to delete ingress")
		return
	}
	a.audit(r, models.AuditActionIngressDelete, "ingress", ingressID, "", "success")
	a.writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// ─── Cache Operation Handlers ──────────────────────────────────

func (a *AdminAPI) createCacheTask(w http.ResponseWriter, r *http.Request, taskType models.TaskType, auditAction models.AuditAction) {
	if ct := r.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(ct, "application/json") {
		a.writeError(w, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE", "Content-Type must be application/json")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBodySize)
	var params models.TaskParams
	if err := json.NewDecoder(r.Body).Decode(&params); err != nil {
		a.writeError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid request body")
		return
	}
	paramsJSON, _ := json.Marshal(params)

	task := &models.Task{
		TenantID:  r.Header.Get("X-Actor-TenantId"),
		CreatorID: r.Header.Get("X-Actor-UserId"),
		Type:      taskType,
		Status:    models.TaskStatusPending,
		Params:    paramsJSON,
	}
	if err := a.pg.CreateTask(r.Context(), task); err != nil {
		a.writeError(w, http.StatusInternalServerError, "CREATE_FAILED", "failed to create task")
		return
	}
	a.audit(r, auditAction, "task", task.TaskID, "", "success")
	a.writeJSON(w, http.StatusCreated, task)
}

func (a *AdminAPI) handlePrewarm(w http.ResponseWriter, r *http.Request) {
	a.createCacheTask(w, r, models.TaskTypePrewarm, models.AuditActionCachePrewarm)
}

func (a *AdminAPI) handlePurge(w http.ResponseWriter, r *http.Request) {
	a.createCacheTask(w, r, models.TaskTypePurge, models.AuditActionCachePurge)
}

func (a *AdminAPI) handleBlock(w http.ResponseWriter, r *http.Request) {
	a.createCacheTask(w, r, models.TaskTypeBlock, models.AuditActionObjectBlock)
}

// ─── Task Handlers ─────────────────────────────────────────────

func (a *AdminAPI) handleListTasks(w http.ResponseWriter, r *http.Request) {
	tenantID := r.Header.Get("X-Actor-TenantId")
	statusFilter := r.URL.Query().Get("status")
	taskType := r.URL.Query().Get("type")
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if limit <= 0 {
		limit = 50
	}

	tasks, total, err := a.pg.ListTasks(r.Context(), tenantID, statusFilter, taskType, limit, offset)
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "LIST_FAILED", "failed to list tasks")
		return
	}
	if tasks == nil {
		tasks = []*models.Task{}
	}
	a.writeJSON(w, http.StatusOK, models.PaginatedResponse{
		Data:    tasks,
		Total:   total,
		Limit:   limit,
		Offset:  offset,
		HasMore: offset+limit < total,
	})
}

func (a *AdminAPI) handleGetTask(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "taskID")
	task, err := a.pg.GetTask(r.Context(), taskID)
	if err != nil {
		a.writeError(w, http.StatusNotFound, "NOT_FOUND", "task not found")
		return
	}
	a.writeJSON(w, http.StatusOK, task)
}

func (a *AdminAPI) handleCancelTask(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "taskID")
	if err := a.pg.UpdateTaskStatus(r.Context(), taskID, models.TaskStatusCancelled, nil, 0, 0); err != nil {
		a.writeError(w, http.StatusInternalServerError, "CANCEL_FAILED", "failed to cancel task")
		return
	}
	a.audit(r, models.AuditActionTaskCancel, "task", taskID, "", "success")
	a.writeJSON(w, http.StatusOK, map[string]string{"status": "cancelled", "task_id": taskID})
}

// ─── Audit Handlers ────────────────────────────────────────────

func (a *AdminAPI) handleQueryAudit(w http.ResponseWriter, r *http.Request) {
	q := models.AuditQuery{
		ActorID:      r.URL.Query().Get("actor_id"),
		Action:       r.URL.Query().Get("action"),
		ResourceType: r.URL.Query().Get("resource_type"),
		ResourceID:   r.URL.Query().Get("resource_id"),
		Result:       r.URL.Query().Get("result"),
		TenantID:     r.Header.Get("X-Actor-TenantId"),
	}
	if limitStr := r.URL.Query().Get("limit"); limitStr != "" {
		q.Limit, _ = strconv.Atoi(limitStr)
	}
	if offsetStr := r.URL.Query().Get("offset"); offsetStr != "" {
		q.Offset, _ = strconv.Atoi(offsetStr)
	}
	events, total, err := a.pg.QueryAuditEvents(r.Context(), q)
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "QUERY_FAILED", "failed to query audit events")
		return
	}
	if events == nil {
		events = []*models.AuditEvent{}
	}
	a.writeJSON(w, http.StatusOK, models.PaginatedResponse{
		Data:    events,
		Total:   total,
		Limit:   q.Limit,
		Offset:  q.Offset,
		HasMore: q.Offset+q.Limit < total,
	})
}

func (a *AdminAPI) handleExportAudit(w http.ResponseWriter, r *http.Request) {
	format := r.URL.Query().Get("format")
	if format == "" {
		format = "csv"
	}

	q := models.AuditQuery{
		ActorID:      r.URL.Query().Get("actor_id"),
		Action:       r.URL.Query().Get("action"),
		ResourceType: r.URL.Query().Get("resource_type"),
		ResourceID:   r.URL.Query().Get("resource_id"),
		Result:       r.URL.Query().Get("result"),
		TenantID:     r.Header.Get("X-Actor-TenantId"),
		Limit:        10000, // max export size
	}

	events, _, err := a.pg.QueryAuditEvents(r.Context(), q)
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "EXPORT_FAILED", "failed to export audit events")
		return
	}

	if format == "json" {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Disposition", `attachment; filename="audit-export.json"`)
		json.NewEncoder(w).Encode(events)
		return
	}

	// CSV export
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="audit-export.csv"`)

	// BOM for Excel compatibility
	w.Write([]byte{0xEF, 0xBB, 0xBF})

	w.Write([]byte("ID,CreatedAt,TenantID,ActorID,ActorEmail,Action,ResourceType,ResourceID,Result,SourceIP,RequestID\n"))
	for _, e := range events {
		line := fmt.Sprintf("%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s\n",
			csvEscape(e.ID),
			e.CreatedAt.Format(time.RFC3339),
			csvEscape(e.TenantID),
			csvEscape(e.ActorID),
			csvEscape(e.ActorEmail),
			csvEscape(string(e.Action)),
			csvEscape(e.ResourceType),
			csvEscape(e.ResourceID),
			csvEscape(e.Result),
			csvEscape(e.SourceIP),
			csvEscape(e.RequestID),
		)
		w.Write([]byte(line))
	}
}

func csvEscape(s string) string {
	if strings.ContainsAny(s, ",\"\n") {
		return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
	}
	return s
}

// ─── Dashboard Handlers ────────────────────────────────────────

func (a *AdminAPI) handleDashboard(w http.ResponseWriter, r *http.Request) {
	tenantID := r.Header.Get("X-Actor-TenantId")
	dm, err := a.pg.GetDashboardData(r.Context(), tenantID)
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "DASHBOARD_FAILED", "failed to get dashboard data")
		return
	}
	a.writeJSON(w, http.StatusOK, dm)
}

// ─── Settings Handlers ─────────────────────────────────────────

func (a *AdminAPI) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	settings := models.AdminConfig{
		OIDCEnabled:      a.cfg.EnableOIDC,
		OIDCProviderURL:  a.cfg.OIDCProviderURL,
		OIDCClientID:     a.cfg.OIDCClientID,
		LocalAuthEnabled: a.cfg.EnableLocalAuth,
		ReadOnlyMode:     false,
		GrafanaURL:       a.cfg.GrafanaURL,
		PrometheusURL:    a.cfg.PrometheusURL,
		LokiURL:          a.cfg.LokiURL,
	}
	a.writeJSON(w, http.StatusOK, settings)
}

func (a *AdminAPI) handleUpdateSettings(w http.ResponseWriter, r *http.Request) {
	if ct := r.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(ct, "application/json") {
		a.writeError(w, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE", "Content-Type must be application/json")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBodySize)
	var settings models.AdminConfig
	if err := json.NewDecoder(r.Body).Decode(&settings); err != nil {
		a.writeError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid request body")
		return
	}
	// Settings are read-only from config; just acknowledge
	a.writeJSON(w, http.StatusOK, settings)
}

// ─── Helpers ───────────────────────────────────────────────────

func (a *AdminAPI) writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		slog.Error("admin write json", "error", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(b)
}

func (a *AdminAPI) writeError(w http.ResponseWriter, status int, code, message string) {
	b, err := json.Marshal(models.ErrorResponse{
		Error: models.ErrorDetail{
			Code:    code,
			Message: message,
		},
	})
	if err != nil {
		slog.Error("admin write error", "error", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(b)
}

func (a *AdminAPI) audit(r *http.Request, action models.AuditAction, resourceType, resourceID, before, result string) {
	actorID := r.Header.Get("X-Actor-UserId")
	actorEmail := r.Header.Get("X-Actor-Email")
	tenantID := r.Header.Get("X-Actor-TenantId")
	requestID := r.Header.Get("X-Request-Id")
	sourceIP := clientIP(r)
	userAgent := r.UserAgent()

	event := &models.AuditEvent{
		TenantID:     tenantID,
		ActorID:      actorID,
		ActorEmail:   actorEmail,
		Action:       action,
		ResourceType: resourceType,
		ResourceID:   resourceID,
		RequestID:    requestID,
		SourceIP:     sourceIP,
		UserAgent:    userAgent,
		Result:       result,
	}
	if err := a.pg.CreateAuditEvent(r.Context(), event); err != nil {
		slog.Warn("failed to create audit event", "err", err, "action", action)
	}
}

// sha256Hash returns the hex-encoded SHA-256 hash of s.
func sha256Hash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

var _ = subtle.ConstantTimeCompare
var _ = uuid.New
