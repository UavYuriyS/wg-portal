package handlers

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/go-pkgz/routegroup"

	"github.com/h44z/wg-portal/internal/app/api/core/request"
	"github.com/h44z/wg-portal/internal/app/api/core/respond"
	"github.com/h44z/wg-portal/internal/app/api/v0/model"
	"github.com/h44z/wg-portal/internal/app/ha"
	"github.com/h44z/wg-portal/internal/config"
	"github.com/h44z/wg-portal/internal/domain"
)

type HaService interface {
	GetStatus(ctx context.Context) (*ha.Status, error)
	Promote(ctx context.Context, nodeId, actor string) error
	RemoveNode(ctx context.Context, nodeId string) error
	CreateJoinToken(ctx context.Context, validFor time.Duration, uses int) (string, error)
	DeleteJoinToken(ctx context.Context, id string) error
}

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
	apiGroup := g.Mount("/ha")
	apiGroup.Use(e.authenticator.LoggedIn(ScopeAdmin))
	apiGroup.Use(e.authenticator.RequireNonDefaultPassword())

	apiGroup.HandleFunc("GET /status", e.handleStatusGet())
	apiGroup.HandleFunc("POST /promote/{id}", e.handlePromotePost())
	apiGroup.HandleFunc("DELETE /nodes/{id}", e.handleNodeDelete())
	apiGroup.HandleFunc("POST /join-tokens", e.handleJoinTokenPost())
	apiGroup.HandleFunc("DELETE /join-tokens/{id}", e.handleJoinTokenDelete())
}

func (e HaEndpoint) orchestratorOnly(w http.ResponseWriter) bool {
	if !e.cfg.Ha.IsOrchestrator() {
		respond.JSON(w, http.StatusBadRequest, model.Error{
			Code: http.StatusBadRequest, Message: "HA management is only available on the orchestrator",
		})
		return false
	}
	return true
}

func haError(w http.ResponseWriter, err error) {
	code := http.StatusInternalServerError
	switch {
	case errors.Is(err, domain.ErrNotFound):
		code = http.StatusNotFound
	case errors.Is(err, domain.ErrInvalidData):
		code = http.StatusBadRequest
	}
	respond.JSON(w, code, model.Error{Code: code, Message: err.Error()})
}

// handleStatusGet returns the HA cluster status.
//
// @ID ha_handleStatusGet
// @Tags HA
// @Summary Get the HA cluster status (nodes, active node, join tokens).
// @Produce json
// @Success 200 {object} model.HaStatus
// @Router /ha/status [get]
func (e HaEndpoint) handleStatusGet() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !e.cfg.Ha.Enabled() {
			respond.JSON(w, http.StatusOK, model.HaStatus{Role: string(config.HaRoleStandalone)})
			return
		}
		status, err := e.ha.GetStatus(r.Context())
		if err != nil {
			haError(w, err)
			return
		}
		respond.JSON(w, http.StatusOK,
			model.NewHaStatus(string(e.cfg.Ha.Role), status.State, status.Nodes, status.Tokens,
				status.ConnectedPeers))
	}
}

// handlePromotePost makes a node the active one and updates DNS.
//
// @ID ha_handlePromotePost
// @Tags HA
// @Summary Promote a node to be the active node and point DNS at it.
// @Param id path string true "The node id"
// @Success 204 "No content"
// @Router /ha/promote/{id} [post]
func (e HaEndpoint) handlePromotePost() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !e.orchestratorOnly(w) {
			return
		}
		actor := string(domain.GetUserInfo(r.Context()).Id)
		if err := e.ha.Promote(r.Context(), request.Path(r, "id"), actor); err != nil {
			haError(w, err)
			return
		}
		respond.Status(w, http.StatusNoContent)
	}
}

// handleNodeDelete removes a standby node from the cluster.
//
// @ID ha_handleNodeDelete
// @Tags HA
// @Summary Remove a (non-active) node from the cluster.
// @Param id path string true "The node id"
// @Success 204 "No content"
// @Router /ha/nodes/{id} [delete]
func (e HaEndpoint) handleNodeDelete() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !e.orchestratorOnly(w) {
			return
		}
		if err := e.ha.RemoveNode(r.Context(), request.Path(r, "id")); err != nil {
			haError(w, err)
			return
		}
		respond.Status(w, http.StatusNoContent)
	}
}

// handleJoinTokenPost creates a join token. The token is only returned once.
//
// @ID ha_handleJoinTokenPost
// @Tags HA
// @Summary Create a node join token.
// @Param request body model.HaJoinTokenRequest true "Validity and number of uses"
// @Produce json
// @Success 200 {object} model.HaJoinTokenResponse
// @Router /ha/join-tokens [post]
func (e HaEndpoint) handleJoinTokenPost() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !e.orchestratorOnly(w) {
			return
		}
		var req model.HaJoinTokenRequest
		if err := request.BodyJson(r, &req); err != nil {
			respond.JSON(w, http.StatusBadRequest, model.Error{Code: http.StatusBadRequest, Message: err.Error()})
			return
		}
		if req.ValidHours <= 0 {
			req.ValidHours = 24
		}
		token, err := e.ha.CreateJoinToken(r.Context(), time.Duration(req.ValidHours)*time.Hour, req.Uses)
		if err != nil {
			haError(w, err)
			return
		}
		respond.JSON(w, http.StatusOK, model.HaJoinTokenResponse{Token: token})
	}
}

// handleJoinTokenDelete revokes a join token.
//
// @ID ha_handleJoinTokenDelete
// @Tags HA
// @Summary Revoke a node join token.
// @Param id path string true "The token id"
// @Success 204 "No content"
// @Router /ha/join-tokens/{id} [delete]
func (e HaEndpoint) handleJoinTokenDelete() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !e.orchestratorOnly(w) {
			return
		}
		if err := e.ha.DeleteJoinToken(r.Context(), request.Path(r, "id")); err != nil {
			haError(w, err)
			return
		}
		respond.Status(w, http.StatusNoContent)
	}
}
