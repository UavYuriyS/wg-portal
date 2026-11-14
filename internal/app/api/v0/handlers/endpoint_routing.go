package handlers

import (
	"context"
	"net/http"

	"github.com/go-pkgz/routegroup"

	"github.com/h44z/wg-portal/internal/app/api/core/request"
	"github.com/h44z/wg-portal/internal/app/api/core/respond"
	"github.com/h44z/wg-portal/internal/app/api/v0/model"
	"github.com/h44z/wg-portal/internal/app/routing"
	"github.com/h44z/wg-portal/internal/domain"
)

type RoutingService interface {
	GetExits(ctx context.Context) ([]routing.ExitInfo, error)
	SetExitCandidates(ctx context.Context, id domain.InterfaceIdentifier, c []domain.ExitCandidate) (
		*routing.ExitInfo, error)
	SetActiveExit(ctx context.Context, id domain.InterfaceIdentifier, peer domain.PeerIdentifier) (
		*routing.ExitInfo, error)
	GetLists(ctx context.Context) ([]domain.RoutingList, error)
	GetList(ctx context.Context, id string) (*domain.RoutingList, error)
	SaveList(ctx context.Context, id string, req routing.ListRequest) (*domain.RoutingList, error)
	DeleteList(ctx context.Context, id string) error
	GetSettings(ctx context.Context) (*domain.RoutingSettings, error)
	SaveSettings(ctx context.Context, enabled bool, upstreams []string) (*domain.RoutingSettings, error)
}

type RoutingEndpoint struct {
	authenticator Authenticator
	routing       RoutingService
}

func NewRoutingEndpoint(authenticator Authenticator, routing RoutingService) RoutingEndpoint {
	return RoutingEndpoint{authenticator: authenticator, routing: routing}
}

func (e RoutingEndpoint) GetName() string {
	return "RoutingEndpoint"
}

func (e RoutingEndpoint) RegisterRoutes(g *routegroup.Bundle) {
	apiGroup := g.Mount("/routing")
	apiGroup.Use(e.authenticator.LoggedIn(ScopeAdmin))
	apiGroup.Use(e.authenticator.RequireNonDefaultPassword())

	apiGroup.HandleFunc("GET /exits", e.handleExitsGet())
	apiGroup.HandleFunc("PUT /exits/{iface}/candidates", e.handleCandidatesPut())
	apiGroup.HandleFunc("PUT /exits/{iface}/active", e.handleActivePut())
	apiGroup.HandleFunc("GET /lists", e.handleListsGet())
	apiGroup.HandleFunc("GET /lists/{id}", e.handleListGet())
	apiGroup.HandleFunc("POST /lists", e.handleListSave(false))
	apiGroup.HandleFunc("PUT /lists/{id}", e.handleListSave(true))
	apiGroup.HandleFunc("DELETE /lists/{id}", e.handleListDelete())
	apiGroup.HandleFunc("GET /settings", e.handleSettingsGet())
	apiGroup.HandleFunc("PUT /settings", e.handleSettingsPut())
}

func badRequest(w http.ResponseWriter, err error) {
	respond.JSON(w, http.StatusBadRequest, model.Error{Code: http.StatusBadRequest, Message: err.Error()})
}

// handleExitsGet returns the exit candidates and the active exit of every server interface.
//
// @ID routing_handleExitsGet
// @Tags Routing
// @Summary Get the exit node configuration of all server interfaces.
// @Produce json
// @Success 200 {object} []model.RoutingExitInterface
// @Router /routing/exits [get]
func (e RoutingEndpoint) handleExitsGet() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		exits, err := e.routing.GetExits(r.Context())
		if err != nil {
			haError(w, err)
			return
		}
		out := make([]model.RoutingExitInterface, len(exits))
		for i, x := range exits {
			out[i] = model.NewRoutingExitInterface(x)
		}
		respond.JSON(w, http.StatusOK, out)
	}
}

// handleCandidatesPut replaces the exit candidates of an interface.
//
// @ID routing_handleCandidatesPut
// @Tags Routing
// @Summary Replace the exit candidates of an interface; removing the active exit deactivates it.
// @Param iface path string true "The interface identifier"
// @Param request body []model.RoutingExitCandidateRequest true "The candidates"
// @Produce json
// @Success 200 {object} model.RoutingExitInterface
// @Router /routing/exits/{iface}/candidates [put]
func (e RoutingEndpoint) handleCandidatesPut() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req []model.RoutingExitCandidateRequest
		if err := request.BodyJson(r, &req); err != nil {
			badRequest(w, err)
			return
		}
		candidates := make([]domain.ExitCandidate, len(req))
		for i, c := range req {
			candidates[i] = domain.ExitCandidate{PeerIdentifier: domain.PeerIdentifier(c.PeerIdentifier),
				EgressIp: c.EgressIp}
		}
		info, err := e.routing.SetExitCandidates(r.Context(), domain.InterfaceIdentifier(request.Path(r, "iface")),
			candidates)
		if err != nil {
			haError(w, err)
			return
		}
		respond.JSON(w, http.StatusOK, model.NewRoutingExitInterface(*info))
	}
}

// handleActivePut selects the active exit of an interface.
//
// @ID routing_handleActivePut
// @Tags Routing
// @Summary Select the active exit node of an interface (empty PeerIdentifier: none).
// @Param iface path string true "The interface identifier"
// @Param request body model.RoutingActiveExitRequest true "The exit candidate"
// @Produce json
// @Success 200 {object} model.RoutingExitInterface
// @Router /routing/exits/{iface}/active [put]
func (e RoutingEndpoint) handleActivePut() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req model.RoutingActiveExitRequest
		if err := request.BodyJson(r, &req); err != nil {
			badRequest(w, err)
			return
		}
		info, err := e.routing.SetActiveExit(r.Context(), domain.InterfaceIdentifier(request.Path(r, "iface")),
			domain.PeerIdentifier(req.PeerIdentifier))
		if err != nil {
			haError(w, err)
			return
		}
		respond.JSON(w, http.StatusOK, model.NewRoutingExitInterface(*info))
	}
}

// handleListsGet returns all routing lists without their content.
//
// @ID routing_handleListsGet
// @Tags Routing
// @Summary Get all bypass and blackhole lists (without content).
// @Produce json
// @Success 200 {object} []model.RoutingList
// @Router /routing/lists [get]
func (e RoutingEndpoint) handleListsGet() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		lists, err := e.routing.GetLists(r.Context())
		if err != nil {
			haError(w, err)
			return
		}
		out := make([]model.RoutingList, len(lists))
		for i, l := range lists {
			out[i] = model.NewRoutingList(l)
		}
		respond.JSON(w, http.StatusOK, out)
	}
}

// handleListGet returns one routing list with its content.
//
// @ID routing_handleListGet
// @Tags Routing
// @Summary Get a bypass or blackhole list with its content.
// @Param id path string true "The list id"
// @Produce json
// @Success 200 {object} model.RoutingList
// @Router /routing/lists/{id} [get]
func (e RoutingEndpoint) handleListGet() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		list, err := e.routing.GetList(r.Context(), request.Path(r, "id"))
		if err != nil {
			haError(w, err)
			return
		}
		respond.JSON(w, http.StatusOK, model.NewRoutingList(*list))
	}
}

// handleListSave creates (POST) or updates (PUT) a routing list. Invalid content is rejected with the
// first 10 errors and their line numbers.
//
// @ID routing_handleListSave
// @Tags Routing
// @Summary Create or update a bypass or blackhole list.
// @Param id path string false "The list id (PUT only)"
// @Param request body model.RoutingListRequest true "The list"
// @Produce json
// @Success 200 {object} model.RoutingList
// @Router /routing/lists [post]
// @Router /routing/lists/{id} [put]
func (e RoutingEndpoint) handleListSave(update bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req model.RoutingListRequest
		if err := request.BodyJson(r, &req); err != nil {
			badRequest(w, err)
			return
		}
		id := ""
		if update {
			id = request.Path(r, "id")
		}
		list, err := e.routing.SaveList(r.Context(), id, routing.ListRequest{Name: req.Name,
			Kind: domain.RoutingListKind(req.Kind), Enabled: req.Enabled, Content: req.Content})
		if err != nil {
			haError(w, err)
			return
		}
		respond.JSON(w, http.StatusOK, model.NewRoutingList(*list))
	}
}

// handleListDelete deletes a routing list.
//
// @ID routing_handleListDelete
// @Tags Routing
// @Summary Delete a bypass or blackhole list.
// @Param id path string true "The list id"
// @Success 204 "No content"
// @Router /routing/lists/{id} [delete]
func (e RoutingEndpoint) handleListDelete() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := e.routing.DeleteList(r.Context(), request.Path(r, "id")); err != nil {
			haError(w, err)
			return
		}
		respond.Status(w, http.StatusNoContent)
	}
}

// handleSettingsGet returns the DNS forwarder settings.
//
// @ID routing_handleSettingsGet
// @Tags Routing
// @Summary Get the DNS forwarder settings.
// @Produce json
// @Success 200 {object} model.RoutingSettings
// @Router /routing/settings [get]
func (e RoutingEndpoint) handleSettingsGet() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s, err := e.routing.GetSettings(r.Context())
		if err != nil {
			haError(w, err)
			return
		}
		respond.JSON(w, http.StatusOK, model.NewRoutingSettings(*s))
	}
}

// handleSettingsPut updates the DNS forwarder settings.
//
// @ID routing_handleSettingsPut
// @Tags Routing
// @Summary Update the DNS forwarder settings (upstreams as host[:port], tried in order).
// @Param request body model.RoutingSettings true "The settings"
// @Produce json
// @Success 200 {object} model.RoutingSettings
// @Router /routing/settings [put]
func (e RoutingEndpoint) handleSettingsPut() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req model.RoutingSettings
		if err := request.BodyJson(r, &req); err != nil {
			badRequest(w, err)
			return
		}
		s, err := e.routing.SaveSettings(r.Context(), req.DnsForwarderEnabled, req.DnsUpstreams)
		if err != nil {
			haError(w, err)
			return
		}
		respond.JSON(w, http.StatusOK, model.NewRoutingSettings(*s))
	}
}
