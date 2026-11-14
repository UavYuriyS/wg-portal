package handlers

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-pkgz/routegroup"

	"github.com/h44z/wg-portal/internal/app/api/core/request"
	"github.com/h44z/wg-portal/internal/app/api/core/respond"
	"github.com/h44z/wg-portal/internal/app/api/v1/models"
	"github.com/h44z/wg-portal/internal/app/ha"
	"github.com/h44z/wg-portal/internal/config"
	"github.com/h44z/wg-portal/internal/domain"
)

type HaService interface {
	Register(ctx context.Context, token string, req ha.RegisterRequest) (*ha.RegisterResponse, error)
	GetStatus(ctx context.Context) (*ha.Status, error)
	Promote(ctx context.Context, nodeId, actor string) error
	CreateJoinToken(ctx context.Context, validFor time.Duration, uses int) (string, error)
}

// HaEndpoint lets HA nodes join the cluster (authenticated by a join token, so nodes never hold an admin
// API token) and lets admins automate the cluster.
type HaEndpoint struct {
	cfg           *config.Config
	authenticator Authenticator
	ha            HaService
}

func NewHaEndpoint(cfg *config.Config, authenticator Authenticator, ha HaService) HaEndpoint {
	return HaEndpoint{cfg: cfg, authenticator: authenticator, ha: ha}
}

func (e HaEndpoint) GetName() string {
	return "HaEndpoint"
}

func (e HaEndpoint) RegisterRoutes(g *routegroup.Bundle) {
	if !e.cfg.Ha.IsOrchestrator() {
		return
	}
	g.HandleFunc("POST /ha/register", e.handleRegisterPost())

	admin := g.Mount("/ha")
	admin.Use(e.authenticator.LoggedIn(ScopeAdmin))
	admin.HandleFunc("GET /status", e.handleStatusGet())
	admin.HandleFunc("POST /promote/{id}", e.handlePromotePost())
	admin.HandleFunc("POST /join-tokens", e.handleJoinTokenPost())
}

// handleStatusGet returns the cluster state, nodes and join tokens (never the CA key or token hashes).
//
// @ID ha_handleStatusGet
// @Tags HA
// @Summary Get the HA cluster status.
// @Produce json
// @Success 200 {object} ha.Status
// @Router /ha/status [get]
// @Security BasicAuth
func (e HaEndpoint) handleStatusGet() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		status, err := e.ha.GetStatus(r.Context())
		if err != nil {
			respond.JSON(w, http.StatusInternalServerError,
				models.Error{Code: http.StatusInternalServerError, Message: err.Error()})
			return
		}
		respond.JSON(w, http.StatusOK, status)
	}
}

// handlePromotePost makes a node the active one and updates DNS.
//
// @ID ha_handlePromotePost
// @Tags HA
// @Summary Promote a node to be the active node.
// @Param id path string true "The node id"
// @Success 204 "No content"
// @Router /ha/promote/{id} [post]
// @Security BasicAuth
func (e HaEndpoint) handlePromotePost() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor := string(domain.GetUserInfo(r.Context()).Id)
		err := e.ha.Promote(r.Context(), request.Path(r, "id"), actor)
		switch {
		case errors.Is(err, domain.ErrNotFound):
			respond.JSON(w, http.StatusNotFound, models.Error{Code: http.StatusNotFound, Message: err.Error()})
		case err != nil:
			respond.JSON(w, http.StatusInternalServerError,
				models.Error{Code: http.StatusInternalServerError, Message: err.Error()})
		default:
			respond.Status(w, http.StatusNoContent)
		}
	}
}

type haJoinTokenRequest struct {
	ValidHours int `json:"ValidHours"`
	Uses       int `json:"Uses"`
}

// handleJoinTokenPost creates a node join token, returned only once.
//
// @ID ha_handleJoinTokenPost
// @Tags HA
// @Summary Create a node join token.
// @Param request body haJoinTokenRequest true "Validity in hours (default 24) and number of uses (default 1)"
// @Produce json
// @Success 200 {object} map[string]string
// @Router /ha/join-tokens [post]
// @Security BasicAuth
func (e HaEndpoint) handleJoinTokenPost() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req haJoinTokenRequest
		if err := request.BodyJson(r, &req); err != nil {
			respond.JSON(w, http.StatusBadRequest, models.Error{Code: http.StatusBadRequest, Message: err.Error()})
			return
		}
		if req.ValidHours <= 0 {
			req.ValidHours = 24
		}
		token, err := e.ha.CreateJoinToken(r.Context(), time.Duration(req.ValidHours)*time.Hour, req.Uses)
		if err != nil {
			respond.JSON(w, http.StatusInternalServerError,
				models.Error{Code: http.StatusInternalServerError, Message: err.Error()})
			return
		}
		respond.JSON(w, http.StatusOK, map[string]string{"Token": token})
	}
}

// handleRegisterPost registers a node.
//
// @ID ha_handleRegisterPost
// @Tags HA
// @Summary Register an HA node (used by "wg-portal -ha-init").
// @Param Authorization header string true "Bearer <join token>"
// @Param request body ha.RegisterRequest true "The node"
// @Produce json
// @Success 200 {object} ha.RegisterResponse
// @Failure 401 {object} models.Error
// @Router /ha/register [post]
func (e HaEndpoint) handleRegisterPost() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || token == "" {
			respond.JSON(w, http.StatusUnauthorized, models.Error{Code: http.StatusUnauthorized, Message: "missing join token"})
			return
		}
		var req ha.RegisterRequest
		if err := request.BodyJson(r, &req); err != nil {
			respond.JSON(w, http.StatusBadRequest, models.Error{Code: http.StatusBadRequest, Message: err.Error()})
			return
		}
		ctx := domain.SetUserInfo(r.Context(), domain.SystemAdminContextUserInfo())
		resp, err := e.ha.Register(ctx, token, req)
		switch {
		case errors.Is(err, ha.ErrInvalidJoinToken):
			slog.Warn("ha: rejected node registration", "node", req.NodeId, "remote", r.RemoteAddr)
			respond.JSON(w, http.StatusUnauthorized, models.Error{Code: http.StatusUnauthorized, Message: err.Error()})
		case errors.Is(err, domain.ErrInvalidData):
			respond.JSON(w, http.StatusBadRequest, models.Error{Code: http.StatusBadRequest, Message: err.Error()})
		case err != nil:
			slog.Error("ha: node registration failed", "node", req.NodeId, "error", err)
			respond.JSON(w, http.StatusInternalServerError,
				models.Error{Code: http.StatusInternalServerError, Message: "registration failed"})
		default:
			respond.JSON(w, http.StatusOK, resp)
		}
	}
}
