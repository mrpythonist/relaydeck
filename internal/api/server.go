// Package api implements the RelayDeck HTTP server: webhook ingestion,
// destination/event/delivery management, health/metrics and the dashboard.
//
// Phase 1 hosted mode (RELAYDECK_HOSTED=true): the same server becomes a
// multi-tenant service. Every management call is scoped to one workspace,
// resolved from a workspace API key or a session cookie plus a workspace
// id. Self-hosted mode (hosted off, the default) keeps the old behaviour:
// workspace "" everywhere, no signup, legacy ingestion unchanged.
package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mrpythonist/relaydeck/internal/auth"
	"github.com/mrpythonist/relaydeck/internal/billing"
	"github.com/mrpythonist/relaydeck/internal/config"
	"github.com/mrpythonist/relaydeck/internal/database"
	"github.com/mrpythonist/relaydeck/internal/delivery"
	"github.com/mrpythonist/relaydeck/internal/events"
	"github.com/mrpythonist/relaydeck/internal/metrics"
	"github.com/mrpythonist/relaydeck/internal/plans"
	"github.com/mrpythonist/relaydeck/internal/security"
	web "github.com/mrpythonist/relaydeck/web"
)

// Server is the HTTP server.
type Server struct {
	Store   *database.Store
	Config  *config.Config
	Metrics *metrics.Metrics
	Sender  *delivery.Sender
	Billing *billing.Client
	mux     *http.ServeMux
	log     *slog.Logger
}

// New builds a Server.
func New(store *database.Store, cfg *config.Config, m *metrics.Metrics) *Server {
	s := &Server{Store: store, Config: cfg, Metrics: m, log: slog.Default(), Billing: billing.New(cfg.StripeSecretKey)}
	s.Sender = delivery.NewSender(cfg.AllowPrivate, cfg.TrustedInternal)
	s.mux = http.NewServeMux()
	s.routes()
	return s
}

func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) routes() {
	m := s.mux
	m.HandleFunc("GET /health", s.handleHealth)
	m.HandleFunc("GET /ready", s.handleReady)
	m.HandleFunc("GET /metrics", s.handleMetrics)
	m.HandleFunc("POST /api/v1/webhooks/{source}", s.handleIngest)
	m.HandleFunc("POST /ingest/{workspace}/{source}", s.handleIngestHosted)
	m.HandleFunc("GET /pricing", s.handlePricing)

	if s.Config.Hosted {
		m.HandleFunc("POST /api/v1/auth/signup", s.handleSignup)
		m.HandleFunc("POST /api/v1/auth/login", s.handleLogin)
		m.HandleFunc("POST /api/v1/auth/logout", s.handleLogout)
		m.HandleFunc("GET /api/v1/auth/me", s.handleMe)
		m.HandleFunc("POST /api/v1/workspaces", s.handleCreateWorkspace)
		m.HandleFunc("GET /api/v1/workspaces", s.handleListWorkspaces)
		m.HandleFunc("GET /api/v1/workspaces/{id}", s.handleGetWorkspace)
		m.HandleFunc("POST /api/v1/workspaces/{id}/api-keys", s.handleCreateWorkspaceAPIKey)
		m.HandleFunc("GET /api/v1/workspaces/{id}/api-keys", s.handleListWorkspaceAPIKeys)
		m.HandleFunc("POST /api/v1/workspaces/{id}/api-keys/{keyID}/revoke", s.handleRevokeWorkspaceAPIKey)
		m.HandleFunc("GET /api/v1/workspaces/{id}/usage", s.handleWorkspaceUsage)
		m.HandleFunc("POST /api/v1/workspaces/{id}/prune", s.handleWorkspacePrune)
		m.HandleFunc("POST /api/v1/billing/checkout", s.handleBillingCheckout)
	}
	// Stripe webhook must be reachable without session auth; its own
	// signature verification is the authentication.
	m.HandleFunc("POST /api/v1/billing/stripe-webhook", s.handleStripeWebhook)

	// Management API: API-key enforced when configured (self-hosted),
	// workspace auth enforced when hosted.
	api := auth.RequireAPIKey(s.Config.APIKey, http.HandlerFunc(s.handleAPI))
	m.Handle("/api/v1/", api)

	// Dashboard.
	m.HandleFunc("GET /{$}", s.handleDashboard)
	m.HandleFunc("GET /dashboard", s.handleDashboard)
	m.HandleFunc("GET /static/", s.handleStatic)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func (s *Server) logReq(r *http.Request, code int, dur time.Duration) {
	s.log.Info("http", "method", r.Method, "path", r.URL.Path, "status", code, "duration_ms", dur.Milliseconds())
}

// --- health / metrics ---

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.Ping(); err != nil {
		writeErr(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	_, _ = w.Write([]byte(s.Metrics.Render(s.Store.PendingCount())))
}

// --- dashboard / pricing ---

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	// Hosted mode: no session means the login/signup page, not the
	// dashboard. The dashboard itself still needs workspace auth for
	// its API calls.
	if s.Config.Hosted {
		if _, err := s.sessionUser(r); err != nil || s.sessionUserOrNil(r) == nil {
			b, err := fs.ReadFile(web.FS, "templates/auth.html")
			if err != nil {
				writeErr(w, http.StatusInternalServerError, "dashboard unavailable")
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write(b)
			return
		}
	}
	b, err := fs.ReadFile(web.FS, "templates/index.html")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "dashboard unavailable")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(b)
}

func (s *Server) handlePricing(w http.ResponseWriter, r *http.Request) {
	if !s.Config.Hosted {
		writeErr(w, http.StatusNotFound, "pricing is only available in hosted mode")
		return
	}
	b, err := fs.ReadFile(web.FS, "templates/pricing.html")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "pricing unavailable")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(b)
}

func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/")
	b, err := fs.ReadFile(web.FS, name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	switch {
	case strings.HasSuffix(name, ".css"):
		w.Header().Set("Content-Type", "text/css")
	case strings.HasSuffix(name, ".js"):
		w.Header().Set("Content-Type", "text/javascript")
	}
	_, _ = w.Write(b)
}

// --- sessions ---

func newSessionToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func (s *Server) sessionUserOrNil(r *http.Request) *database.User {
	u, _ := s.sessionUser(r)
	return u
}

func (s *Server) sessionUser(r *http.Request) (*database.User, error) {
	c, err := r.Cookie("relaydeck_session")
	if err != nil || c.Value == "" {
		return nil, fmt.Errorf("no session")
	}
	u, err := s.Store.GetSessionUser(database.HashToken(c.Value))
	if err != nil {
		return nil, err
	}
	if u == nil {
		return nil, fmt.Errorf("invalid or expired session")
	}
	return u, nil
}

func (s *Server) setSessionCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name: "relaydeck_session", Value: token, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, MaxAge: 7 * 24 * 3600,
		// Secure is false so local development over plain HTTP works.
		// Behind a TLS-terminating proxy this cookie still travels over
		// HTTPS to the browser; set Secure=true when serving TLS directly.
	})
}

func (s *Server) createSessionForUser(w http.ResponseWriter, userID string) error {
	token, err := newSessionToken()
	if err != nil {
		return err
	}
	if err := s.Store.CreateSession(database.HashToken(token), userID, time.Now().UTC().Add(7*24*time.Hour)); err != nil {
		return err
	}
	s.setSessionCookie(w, token)
	return nil
}

// authContext is the resolved caller for a hosted management request.
type authContext struct {
	User      *database.User
	Workspace *database.Workspace
}

// resolveAuth resolves a hosted caller. It accepts, in order:
//  1. a workspace API key in X-API-Key / Authorization: Bearer, which
//     implies its workspace; 2. a session cookie plus a workspace id in
//     the X-Workspace-Id header or ?workspace_id=, checked for membership.
//
// In self-hosted mode it returns an empty workspace ("").
func (s *Server) resolveAuth(r *http.Request) (*authContext, error) {
	if !s.Config.Hosted {
		return &authContext{Workspace: &database.Workspace{ID: ""}}, nil
	}
	// Workspace API key.
	if key := auth.APIKeyFromRequest(r); key != "" {
		if ws, err := s.Store.GetWorkspaceByAPIKey(key); err == nil && ws != nil {
			return &authContext{Workspace: ws}, nil
		}
	}
	// Session + explicit workspace.
	user, err := s.sessionUser(r)
	if err != nil || user == nil {
		return nil, fmt.Errorf("authentication required: log in or send a workspace API key")
	}
	wsID := r.Header.Get("X-Workspace-Id")
	if wsID == "" {
		wsID = r.URL.Query().Get("workspace_id")
	}
	if wsID == "" {
		// Default to the user's first workspace.
		wss, err := s.Store.ListWorkspacesForUser(user.ID)
		if err != nil || len(wss) == 0 {
			return nil, fmt.Errorf("no workspace found for this account")
		}
		return &authContext{User: user, Workspace: wss[0]}, nil
	}
	ws, err := s.Store.GetWorkspace(wsID)
	if err != nil || ws == nil {
		return nil, fmt.Errorf("workspace not found")
	}
	ok, err := s.Store.IsMember(ws.ID, user.ID)
	if err != nil || !ok {
		return nil, fmt.Errorf("not a member of this workspace")
	}
	return &authContext{User: user, Workspace: ws}, nil
}

func (s *Server) workspaceID(r *http.Request) string {
	ac, err := s.resolveAuth(r)
	if err != nil || ac == nil || ac.Workspace == nil {
		return ""
	}
	return ac.Workspace.ID
}

// --- auth endpoints (hosted only) ---

func (s *Server) handleSignup(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email    string `json:"email"`
		Password string `json:"password"`
		Name     string `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	in.Email = strings.ToLower(strings.TrimSpace(in.Email))
	if !auth.ValidateEmail(in.Email) {
		writeErr(w, http.StatusBadRequest, "a valid email address is required")
		return
	}
	if len(in.Password) < 8 {
		writeErr(w, http.StatusBadRequest, "password must be at least 8 characters")
		return
	}
	if existing, _ := s.Store.GetUserByEmail(in.Email); existing != nil {
		writeErr(w, http.StatusConflict, "an account with this email already exists")
		return
	}
	hash, err := auth.HashPassword(in.Password)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	userID, err := security.NewID("usr_")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	user := &database.User{ID: userID, Email: in.Email, PasswordHash: hash, Name: strings.TrimSpace(in.Name)}
	if err := s.Store.CreateUser(user); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	wsID, err := security.NewID("ws_")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	pubID, err := security.NewID("pub_")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	wsName := strings.TrimSpace(in.Name)
	if wsName == "" {
		wsName = strings.Split(in.Email, "@")[0] + " workspace"
	}
	ws := &database.Workspace{ID: wsID, PublicID: pubID, Name: wsName, OwnerUserID: user.ID, Plan: "free"}
	if err := s.Store.CreateWorkspace(ws); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = s.Store.AddMembership(ws.ID, user.ID, "owner")
	if err := s.createSessionForUser(w, user.ID); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"user":      map[string]string{"id": user.ID, "email": user.Email, "name": user.Name},
		"workspace": ws,
	})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	user, err := s.Store.GetUserByEmail(in.Email)
	if err != nil || user == nil || !auth.CheckPassword(in.Password, user.PasswordHash) {
		writeErr(w, http.StatusUnauthorized, "invalid email or password")
		return
	}
	if err := s.createSessionForUser(w, user.ID); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": map[string]string{"id": user.ID, "email": user.Email, "name": user.Name}})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("relaydeck_session"); err == nil && c.Value != "" {
		_ = s.Store.DeleteSession(database.HashToken(c.Value))
	}
	http.SetCookie(w, &http.Cookie{Name: "relaydeck_session", Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	writeJSON(w, http.StatusOK, map[string]string{"status": "logged out"})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	user, err := s.sessionUser(r)
	if err != nil || user == nil {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	wss, _ := s.Store.ListWorkspacesForUser(user.ID)
	if wss == nil {
		wss = []*database.Workspace{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"user":       map[string]string{"id": user.ID, "email": user.Email, "name": user.Name},
		"workspaces": wss,
	})
}

// --- workspace endpoints (hosted only) ---

func (s *Server) requireWorkspaceMember(w http.ResponseWriter, r *http.Request, wsID string) (*database.User, *database.Workspace, bool) {
	user, err := s.sessionUser(r)
	if err != nil || user == nil {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return nil, nil, false
	}
	ws, err := s.Store.GetWorkspace(wsID)
	if err != nil || ws == nil {
		writeErr(w, http.StatusNotFound, "workspace not found")
		return nil, nil, false
	}
	ok, err := s.Store.IsMember(ws.ID, user.ID)
	if err != nil || !ok {
		writeErr(w, http.StatusForbidden, "not a member of this workspace")
		return nil, nil, false
	}
	return user, ws, true
}

func (s *Server) handleCreateWorkspace(w http.ResponseWriter, r *http.Request) {
	user, err := s.sessionUser(r)
	if err != nil || user == nil {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var in struct {
		Name string `json:"name"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&in)
	if strings.TrimSpace(in.Name) == "" {
		writeErr(w, http.StatusBadRequest, "name is required")
		return
	}
	wsID, _ := security.NewID("ws_")
	pubID, _ := security.NewID("pub_")
	ws := &database.Workspace{ID: wsID, PublicID: pubID, Name: strings.TrimSpace(in.Name), OwnerUserID: user.ID, Plan: "free"}
	if err := s.Store.CreateWorkspace(ws); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = s.Store.AddMembership(ws.ID, user.ID, "owner")
	writeJSON(w, http.StatusCreated, ws)
}

func (s *Server) handleListWorkspaces(w http.ResponseWriter, r *http.Request) {
	user, err := s.sessionUser(r)
	if err != nil || user == nil {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	wss, _ := s.Store.ListWorkspacesForUser(user.ID)
	if wss == nil {
		wss = []*database.Workspace{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"workspaces": wss})
}

func (s *Server) handleGetWorkspace(w http.ResponseWriter, r *http.Request) {
	_, ws, ok := s.requireWorkspaceMember(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, ws)
}

func (s *Server) handleCreateWorkspaceAPIKey(w http.ResponseWriter, r *http.Request) {
	_, ws, ok := s.requireWorkspaceMember(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	var in struct {
		Name string `json:"name"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&in)
	if strings.TrimSpace(in.Name) == "" {
		in.Name = "default"
	}
	keyID, _ := security.NewID("key_")
	plain, err := security.NewID("rdk_")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	// Make the key longer than NewID's 24 hex chars: append a second random part.
	extra, _ := security.NewID("")
	plain = plain + extra
	k, err := s.Store.CreateWorkspaceAPIKey(keyID, ws.ID, strings.TrimSpace(in.Name), plain)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": k.ID, "workspace_id": k.WorkspaceID, "name": k.Name, "key_prefix": k.KeyPrefix,
		"key": plain, "note": "Save this key now. It is shown once and cannot be recovered.",
	})
}

func (s *Server) handleListWorkspaceAPIKeys(w http.ResponseWriter, r *http.Request) {
	_, ws, ok := s.requireWorkspaceMember(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	keys, _ := s.Store.ListWorkspaceAPIKeys(ws.ID)
	if keys == nil {
		keys = []*database.WorkspaceAPIKey{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"api_keys": keys})
}

func (s *Server) handleRevokeWorkspaceAPIKey(w http.ResponseWriter, r *http.Request) {
	_, ws, ok := s.requireWorkspaceMember(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	revoked, err := s.Store.RevokeWorkspaceAPIKey(ws.ID, r.PathValue("keyID"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !revoked {
		writeErr(w, http.StatusNotFound, "api key not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
}

func (s *Server) handleWorkspaceUsage(w http.ResponseWriter, r *http.Request) {
	_, ws, ok := s.requireWorkspaceMember(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	usage, _ := s.Store.GetUsage(ws.ID)
	plan := plans.Get(ws.Plan)
	var used int64
	if usage != nil {
		used = usage.EventCount
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"workspace_id": ws.ID, "plan": plan.Name, "price_dollars": plan.PriceDollars,
		"month": usage.Month, "events_used": used, "events_limit": plan.MonthlyEvents,
		"events_remaining": plan.MonthlyEvents - used,
		"max_destinations": plan.MaxDestinations, "retention_days": plan.RetentionDays,
	})
}

func (s *Server) handleWorkspacePrune(w http.ResponseWriter, r *http.Request) {
	_, ws, ok := s.requireWorkspaceMember(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	plan := plans.Get(ws.Plan)
	n, err := s.Store.DeleteOldEventsForWorkspace(ws.ID, plan.RetentionDays)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"workspace_id": ws.ID, "retention_days": plan.RetentionDays, "events_deleted": n})
}

// --- billing endpoints (hosted only, except the Stripe webhook) ---

func (s *Server) handleBillingCheckout(w http.ResponseWriter, r *http.Request) {
	var in struct {
		WorkspaceID string `json:"workspace_id"`
		Plan        string `json:"plan"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	_, ws, ok := s.requireWorkspaceMember(w, r, in.WorkspaceID)
	if !ok {
		return
	}
	if !plans.IsPaid(in.Plan) {
		writeErr(w, http.StatusBadRequest, "plan must be starter or growth")
		return
	}
	if !s.Billing.Configured() {
		writeErr(w, http.StatusServiceUnavailable, "billing is not configured: set STRIPE_SECRET_KEY and STRIPE_PRICE_STARTER / STRIPE_PRICE_GROWTH")
		return
	}
	priceID := s.Config.StripePriceStarter
	if in.Plan == "growth" {
		priceID = s.Config.StripePriceGrowth
	}
	successURL := s.Config.StripeSuccessURL
	if successURL == "" {
		successURL = "https://" + r.Host + "/dashboard?billing=success"
	}
	cancelURL := s.Config.StripeCancelURL
	if cancelURL == "" {
		cancelURL = "https://" + r.Host + "/pricing?billing=cancelled"
	}
	checkoutURL, err := s.Billing.CreateCheckoutSession(priceID, ws.ID, in.Plan, successURL, cancelURL)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"checkout_url": checkoutURL})
}

func (s *Server) handleStripeWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "failed to read body")
		return
	}
	if s.Config.StripeWebhookSecret != "" {
		if !billing.VerifyWebhookSignature(body, r.Header.Get("Stripe-Signature"), s.Config.StripeWebhookSecret) {
			writeErr(w, http.StatusUnauthorized, "invalid stripe signature")
			return
		}
	} else {
		// No secret configured: this is a test/dev setup only. Never
		// run this in production without STRIPE_WEBHOOK_SECRET.
		s.log.Warn("stripe webhook received without a configured webhook secret; signature not verified")
	}
	ev, err := billing.ParseWebhookEvent(body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid event: "+err.Error())
		return
	}
	wsID, plan, customerID, subscriptionID := billing.PlanForEvent(ev)
	switch ev.Type {
	case "checkout.session.completed":
		if wsID == "" || !plans.Valid(plan) {
			writeErr(w, http.StatusBadRequest, "event is missing workspace_id or plan metadata")
			return
		}
		if err := s.Store.UpdateWorkspacePlan(wsID, plan, customerID, subscriptionID); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	case "customer.subscription.deleted":
		if wsID == "" {
			// Subscription object may not carry workspace metadata;
			// without it we cannot safely change a plan.
			writeJSON(w, http.StatusOK, map[string]string{"status": "ignored: no workspace_id in event"})
			return
		}
		_ = s.Store.UpdateWorkspacePlan(wsID, "free", customerID, "")
	default:
		// Unknown event types are acknowledged, not retried by Stripe.
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// --- ingestion ---

func (s *Server) handleIngest(w http.ResponseWriter, r *http.Request) {
	if s.Config.Hosted {
		writeErr(w, http.StatusGone, "legacy ingestion is disabled in hosted mode; use POST /ingest/{workspace_public_id}/{source}")
		return
	}
	s.ingestForWorkspace(w, r, "", r.PathValue("source"))
}

func (s *Server) handleIngestHosted(w http.ResponseWriter, r *http.Request) {
	publicID := r.PathValue("workspace")
	ws, err := s.Store.GetWorkspaceByPublicID(publicID)
	if err != nil || ws == nil {
		writeErr(w, http.StatusNotFound, "workspace not found")
		return
	}
	// Plan limit, checked before accepting the event.
	plan := plans.Get(ws.Plan)
	used, err := s.Store.MonthlyEventCount(ws.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	if used >= plan.MonthlyEvents {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"error": "monthly event limit reached for plan " + plan.Name,
			"plan":  plan.Name, "limit": plan.MonthlyEvents, "used": used,
		})
		return
	}
	s.ingestForWorkspace(w, r, ws.ID, r.PathValue("source"))
}

func (s *Server) ingestForWorkspace(w http.ResponseWriter, r *http.Request, workspaceID, rawSource string) {
	start := time.Now()
	source := events.NormaliseSource(rawSource)
	if err := events.ValidateSourceName(source); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, s.Config.MaxPayloadBytes+1))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "failed to read body")
		return
	}
	if int64(len(body)) > s.Config.MaxPayloadBytes {
		writeErr(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("payload exceeds limit of %d bytes", s.Config.MaxPayloadBytes))
		return
	}
	secret, found, err := s.Store.GetSourceSecretForWorkspace(workspaceID, source)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	if !found {
		_ = s.Store.UpsertSourceForWorkspace(workspaceID, source, "")
	}
	if secret != "" {
		sig := r.Header.Get("X-RelayDeck-Signature")
		if sig == "" {
			sig = r.Header.Get("X-Hub-Signature-256")
		}
		if !security.VerifySignature(secret, body, sig) {
			writeErr(w, http.StatusUnauthorized, "invalid or missing signature")
			return
		}
	}
	headers := map[string]string{}
	for k, vs := range r.Header {
		if len(vs) > 0 {
			headers[k] = vs[0]
		}
	}
	headers = security.RedactHeaders(headers)
	id, err := security.NewEventID()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	ev := &database.Event{
		ID:             id,
		WorkspaceID:    workspaceID,
		Source:         source,
		Payload:        body,
		Headers:        headers,
		ContentType:    r.Header.Get("Content-Type"),
		IdempotencyKey: r.Header.Get("Idempotency-Key"),
	}
	dests, err := s.Store.MatchingDestinationsForWorkspace(workspaceID, source, body)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	existed, err := s.Store.CreateEvent(ev, dests)
	if err != nil {
		s.log.Error("create event failed", "error", err)
		writeErr(w, http.StatusInternalServerError, "failed to persist event")
		return
	}
	if !existed {
		s.Metrics.EventsReceived.Add(1)
	}
	s.logReq(r, http.StatusAccepted, time.Since(start))
	writeJSON(w, http.StatusAccepted, map[string]any{
		"event_id":          ev.ID,
		"status":            "accepted",
		"duplicate":         existed,
		"deliveries_queued": len(dests),
	})
}

// --- management API ---

func (s *Server) handleAPI(w http.ResponseWriter, r *http.Request) {
	// In hosted mode every management call must resolve to a workspace.
	// In self-hosted mode resolveAuth returns workspace "".
	ac, err := s.resolveAuth(r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, err.Error())
		return
	}
	wsID := ""
	if ac != nil && ac.Workspace != nil {
		wsID = ac.Workspace.ID
	}
	// Store the resolved workspace on the request context via a header
	// the handlers below read through s.workspaceIDFrom(r, wsID).
	r.Header.Set("X-RelayDeck-Resolved-Workspace", wsID)
	_ = wsID
	path := r.URL.Path
	switch {
	case path == "/api/v1/destinations" && r.Method == http.MethodGet:
		s.listDestinations(w, r)
	case path == "/api/v1/destinations" && r.Method == http.MethodPost:
		s.createDestination(w, r)
	case strings.HasPrefix(path, "/api/v1/destinations/") && r.Method == http.MethodGet:
		s.getDestination(w, r)
	case strings.HasPrefix(path, "/api/v1/destinations/") && (r.Method == http.MethodPut || r.Method == http.MethodPatch):
		s.updateDestination(w, r)
	case strings.HasPrefix(path, "/api/v1/destinations/") && r.Method == http.MethodDelete:
		s.deleteDestination(w, r)
	case path == "/api/v1/events" && r.Method == http.MethodGet:
		s.listEvents(w, r)
	case strings.HasPrefix(path, "/api/v1/events/") && strings.HasSuffix(path, "/replay") && r.Method == http.MethodPost:
		s.replayEvent(w, r)
	case strings.HasPrefix(path, "/api/v1/events/") && r.Method == http.MethodGet:
		s.getEvent(w, r)
	case path == "/api/v1/deliveries" && r.Method == http.MethodGet:
		s.listDeliveries(w, r)
	case strings.HasPrefix(path, "/api/v1/deliveries/") && strings.HasSuffix(path, "/retry") && r.Method == http.MethodPost:
		s.retryDelivery(w, r)
	case strings.HasPrefix(path, "/api/v1/deliveries/") && r.Method == http.MethodGet:
		s.getDelivery(w, r)
	case path == "/api/v1/stats" && r.Method == http.MethodGet:
		s.getStats(w, r)
	case path == "/api/v1/sources" && r.Method == http.MethodGet:
		s.listSources(w, r)
	default:
		writeErr(w, http.StatusNotFound, "not found")
	}
}

// resolvedWorkspaceID returns the workspace resolved by handleAPI.
func (s *Server) resolvedWorkspaceID(r *http.Request) string {
	return r.Header.Get("X-RelayDeck-Resolved-Workspace")
}

func maskDestination(d *database.Destination) *database.Destination {
	if d == nil {
		return nil
	}
	cp := *d
	cp.Headers = map[string]string{}
	for k, v := range d.Headers {
		if security.IsSensitiveHeader(k) {
			cp.Headers[k] = "***REDACTED***"
		} else {
			cp.Headers[k] = v
		}
	}
	// The signing secret is never returned in clear. Clients set it by
	// sending a new value; sending "***REDACTED***" back leaves it unchanged.
	if cp.SigningSecret != "" {
		cp.SigningSecret = "***REDACTED***"
	}
	cp.HeaderTemplates = map[string]string{}
	for k, v := range d.HeaderTemplates {
		if security.IsSensitiveHeader(k) {
			cp.HeaderTemplates[k] = "***REDACTED***"
		} else {
			cp.HeaderTemplates[k] = v
		}
	}
	return &cp
}

func (s *Server) listDestinations(w http.ResponseWriter, r *http.Request) {
	dests, err := s.Store.ListDestinationsForWorkspace(s.resolvedWorkspaceID(r))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]*database.Destination, 0, len(dests))
	for _, d := range dests {
		out = append(out, maskDestination(d))
	}
	writeJSON(w, http.StatusOK, map[string]any{"destinations": out})
}

func (s *Server) createDestination(w http.ResponseWriter, r *http.Request) {
	wsID := s.resolvedWorkspaceID(r)
	// Destination cap (Free plan only; paid plans have no cap, MaxDestinations 0).
	if s.Config.Hosted && wsID != "" {
		ws, err := s.Store.GetWorkspace(wsID)
		if err == nil && ws != nil {
			plan := plans.Get(ws.Plan)
			if plan.MaxDestinations > 0 {
				n, _ := s.Store.CountDestinationsForWorkspace(wsID)
				if n >= plan.MaxDestinations {
					writeJSON(w, http.StatusTooManyRequests, map[string]any{
						"error": "destination limit reached for plan " + plan.Name,
						"plan":  plan.Name, "limit": plan.MaxDestinations, "used": n,
					})
					return
				}
			}
		}
	}
	var d database.Destination
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&d); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if strings.TrimSpace(d.Name) == "" {
		writeErr(w, http.StatusBadRequest, "name is required")
		return
	}
	if err := security.ValidateDestinationURL(d.URL, s.Config.AllowPrivate, s.Config.TrustedInternal, nil); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid destination url: "+err.Error())
		return
	}
	if d.Method == "" {
		d.Method = "POST"
	}
	d.Method = strings.ToUpper(d.Method)
	if d.TimeoutSeconds <= 0 {
		d.TimeoutSeconds = 10
	}
	if d.MaxRetries <= 0 {
		d.MaxRetries = 5
	}
	if d.BackoffBaseMs <= 0 {
		d.BackoffBaseMs = 1000
	}
	if d.BackoffMaxMs <= 0 {
		d.BackoffMaxMs = 60000
	}
	if d.Headers == nil {
		d.Headers = map[string]string{}
	}
	d.Enabled = true
	d.WorkspaceID = wsID
	id, err := security.NewID("dst_")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	d.ID = id
	if err := s.Store.CreateDestination(&d); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			writeErr(w, http.StatusConflict, "destination name already exists")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, maskDestination(&d))
}

func (s *Server) destID(r *http.Request) string {
	return strings.TrimPrefix(strings.TrimSuffix(r.URL.Path, "/retry"), "/api/v1/destinations/")
}

func (s *Server) getDestination(w http.ResponseWriter, r *http.Request) {
	d, err := s.Store.GetDestinationForWorkspace(s.destID(r), s.resolvedWorkspaceID(r))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if d == nil {
		writeErr(w, http.StatusNotFound, "destination not found")
		return
	}
	writeJSON(w, http.StatusOK, maskDestination(d))
}

func (s *Server) updateDestination(w http.ResponseWriter, r *http.Request) {
	id := s.destID(r)
	wsID := s.resolvedWorkspaceID(r)
	existing, err := s.Store.GetDestinationForWorkspace(id, wsID)
	if err != nil || existing == nil {
		writeErr(w, http.StatusNotFound, "destination not found")
		return
	}
	var d database.Destination
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&d); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if d.URL != "" && d.URL != existing.URL {
		if err := security.ValidateDestinationURL(d.URL, s.Config.AllowPrivate, s.Config.TrustedInternal, nil); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid destination url: "+err.Error())
			return
		}
		existing.URL = d.URL
	}
	if d.Name != "" {
		existing.Name = d.Name
	}
	if d.SourceFilter != "" || r.URL.Query().Has("clear_source") {
		existing.SourceFilter = d.SourceFilter
	}
	if d.Method != "" {
		existing.Method = strings.ToUpper(d.Method)
	}
	if d.TimeoutSeconds > 0 {
		existing.TimeoutSeconds = d.TimeoutSeconds
	}
	if d.MaxRetries > 0 {
		existing.MaxRetries = d.MaxRetries
	}
	if d.BackoffBaseMs > 0 {
		existing.BackoffBaseMs = d.BackoffBaseMs
	}
	if d.BackoffMaxMs > 0 {
		existing.BackoffMaxMs = d.BackoffMaxMs
	}
	if d.Filter != "" {
		existing.Filter = d.Filter
	}
	if r.URL.Query().Has("clear_filter") {
		existing.Filter = ""
	}
	if d.JSONPathFilter != "" {
		existing.JSONPathFilter = d.JSONPathFilter
	}
	if r.URL.Query().Has("clear_jsonpath") {
		existing.JSONPathFilter = ""
	}
	if d.SigningSecret != "" && d.SigningSecret != "***REDACTED***" {
		existing.SigningSecret = d.SigningSecret
	}
	if r.URL.Query().Has("clear_signing_secret") {
		existing.SigningSecret = ""
	}
	if d.HeaderTemplates != nil {
		if existing.HeaderTemplates == nil {
			existing.HeaderTemplates = map[string]string{}
		}
		for k, v := range d.HeaderTemplates {
			if v == "***REDACTED***" {
				continue
			}
			existing.HeaderTemplates[k] = v
		}
	}
	if r.URL.Query().Has("clear_header_templates") {
		existing.HeaderTemplates = map[string]string{}
	}
	if d.Headers != nil {
		if existing.Headers == nil {
			existing.Headers = map[string]string{}
		}
		for k, v := range d.Headers {
			if v == "***REDACTED***" {
				continue
			}
			existing.Headers[k] = v
		}
	}
	existing.Enabled = d.Enabled
	if err := s.Store.UpdateDestination(existing); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, maskDestination(existing))
}

func (s *Server) deleteDestination(w http.ResponseWriter, r *http.Request) {
	ok, err := s.Store.DeleteDestinationForWorkspace(s.destID(r), s.resolvedWorkspaceID(r))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		writeErr(w, http.StatusNotFound, "destination not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func parseLimit(r *http.Request) int {
	n, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil || n <= 0 {
		return 50
	}
	if n > 200 {
		return 200
	}
	return n
}

func (s *Server) listEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	evs, err := s.Store.ListEventsForWorkspace(s.resolvedWorkspaceID(r), parseLimit(r), q.Get("source"), q.Get("status"), q.Get("search"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if evs == nil {
		evs = []*database.Event{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": evs})
}

func eventID(r *http.Request) string {
	p := strings.TrimPrefix(r.URL.Path, "/api/v1/events/")
	return strings.TrimSuffix(p, "/replay")
}

func (s *Server) getEvent(w http.ResponseWriter, r *http.Request) {
	wsID := s.resolvedWorkspaceID(r)
	ev, err := s.Store.GetEventForWorkspace(eventID(r), wsID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if ev == nil {
		writeErr(w, http.StatusNotFound, "event not found")
		return
	}
	dels, _ := s.Store.ListDeliveriesForEvent(wsID, ev.ID)
	type deliveryWithAttempts struct {
		*database.Delivery
		AttemptsList []*database.Attempt `json:"attempts_list"`
	}
	var out []deliveryWithAttempts
	for _, d := range dels {
		atts, _ := s.Store.ListAttempts(d.ID)
		out = append(out, deliveryWithAttempts{d, atts})
	}
	writeJSON(w, http.StatusOK, map[string]any{"event": ev, "deliveries": out})
}

func (s *Server) replayEvent(w http.ResponseWriter, r *http.Request) {
	wsID := s.resolvedWorkspaceID(r)
	ev, err := s.Store.GetEventForWorkspace(eventID(r), wsID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if ev == nil {
		writeErr(w, http.StatusNotFound, "event not found")
		return
	}
	dests, err := s.Store.MatchingDestinationsForWorkspace(wsID, ev.Source, ev.Payload)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	n, err := s.Store.ReplayEvent(ev.ID, dests)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"event_id": ev.ID, "deliveries_created": n})
}

func (s *Server) listDeliveries(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	dels, err := s.Store.ListDeliveriesForWorkspace(s.resolvedWorkspaceID(r), parseLimit(r), q.Get("status"), q.Get("destination_id"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if dels == nil {
		dels = []*database.Delivery{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"deliveries": dels})
}

func deliveryID(r *http.Request) string {
	p := strings.TrimPrefix(r.URL.Path, "/api/v1/deliveries/")
	return strings.TrimSuffix(p, "/retry")
}

func (s *Server) getDelivery(w http.ResponseWriter, r *http.Request) {
	d, err := s.Store.GetDeliveryForWorkspace(deliveryID(r), s.resolvedWorkspaceID(r))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if d == nil {
		writeErr(w, http.StatusNotFound, "delivery not found")
		return
	}
	atts, _ := s.Store.ListAttempts(d.ID)
	writeJSON(w, http.StatusOK, map[string]any{"delivery": d, "attempts": atts})
}

func (s *Server) retryDelivery(w http.ResponseWriter, r *http.Request) {
	ok, err := s.Store.RetryDeliveryForWorkspace(deliveryID(r), s.resolvedWorkspaceID(r))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		writeErr(w, http.StatusNotFound, "delivery not found or not retryable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "queued for retry"})
}

func (s *Server) getStats(w http.ResponseWriter, r *http.Request) {
	st, err := s.Store.StatsForWorkspace(s.resolvedWorkspaceID(r))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) listSources(w http.ResponseWriter, r *http.Request) {
	names, _ := s.Store.ListSourcesForWorkspace(s.resolvedWorkspaceID(r))
	if names == nil {
		names = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"sources": names})
}
