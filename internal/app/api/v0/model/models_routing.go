package model

import (
	"strings"
	"time"

	"github.com/h44z/wg-portal/internal/app/routing"
	"github.com/h44z/wg-portal/internal/domain"
)

type RoutingExitCandidate struct {
	PeerIdentifier string `json:"PeerIdentifier"`
	DisplayName    string `json:"DisplayName"`
	EgressIp       string `json:"EgressIp"`
}

type RoutingExitInterface struct {
	InterfaceIdentifier  string                 `json:"InterfaceIdentifier"`
	ActivePeerIdentifier string                 `json:"ActivePeerIdentifier"` // empty: no exit active
	Candidates           []RoutingExitCandidate `json:"Candidates"`
	// Detected: peers with 0.0.0.0/0 or ::/0 that are no candidates (EgressIp empty)
	Detected []RoutingExitCandidate `json:"Detected"`
}

type RoutingExitCandidateRequest struct {
	PeerIdentifier string `json:"PeerIdentifier"`
	EgressIp       string `json:"EgressIp"`
}

type RoutingActiveExitRequest struct {
	PeerIdentifier string `json:"PeerIdentifier"` // empty: no exit
}

type RoutingList struct {
	Id         string    `json:"Id"`
	Name       string    `json:"Name"`
	Kind       string    `json:"Kind"` // blackhole or bypass
	Enabled    bool      `json:"Enabled"`
	Content    string    `json:"Content,omitempty"` // only in GET /routing/lists/{id}
	EntryCount int       `json:"EntryCount"`
	UpdatedAt  time.Time `json:"UpdatedAt"`
	UpdatedBy  string    `json:"UpdatedBy"`
}

type RoutingListRequest struct {
	Name    string  `json:"Name"`
	Kind    string  `json:"Kind"`
	Enabled bool    `json:"Enabled"`
	Content *string `json:"Content"` // omitted or null on PUT: keep the current content
}

type RoutingSettings struct {
	DnsForwarderEnabled bool     `json:"DnsForwarderEnabled"`
	DnsUpstreams        []string `json:"DnsUpstreams"`
}

func NewRoutingExitInterface(e routing.ExitInfo) RoutingExitInterface {
	out := RoutingExitInterface{
		InterfaceIdentifier:  string(e.InterfaceIdentifier),
		ActivePeerIdentifier: string(e.ActivePeerIdentifier),
		Candidates:           make([]RoutingExitCandidate, len(e.Candidates)),
		Detected:             make([]RoutingExitCandidate, len(e.Detected)),
	}
	for i, c := range e.Candidates {
		out.Candidates[i] = RoutingExitCandidate{PeerIdentifier: string(c.PeerIdentifier), DisplayName: c.DisplayName,
			EgressIp: c.EgressIp}
	}
	for i, c := range e.Detected {
		out.Detected[i] = RoutingExitCandidate{PeerIdentifier: string(c.PeerIdentifier), DisplayName: c.DisplayName}
	}
	return out
}

func NewRoutingList(l domain.RoutingList) RoutingList {
	return RoutingList{Id: l.Id, Name: l.Name, Kind: string(l.Kind), Enabled: l.Enabled, Content: l.Content,
		EntryCount: l.EntryCount, UpdatedAt: l.UpdatedAt, UpdatedBy: l.UpdatedBy}
}

func NewRoutingSettings(s domain.RoutingSettings) RoutingSettings {
	upstreams := []string{}
	for _, u := range strings.Split(s.DnsUpstreams, ",") {
		if u = strings.TrimSpace(u); u != "" {
			upstreams = append(upstreams, u)
		}
	}
	return RoutingSettings{DnsForwarderEnabled: s.DnsForwarderEnabled, DnsUpstreams: upstreams}
}
