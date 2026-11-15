package wireguard

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/h44z/wg-portal/internal/app"
	"github.com/h44z/wg-portal/internal/domain"
)

// HaReconciler applies the replicated database to the local kernel on HA nodes. Unlike
// RestoreInterfaceState it never writes to the database: a node that wrote "interface missing" into the
// shared DB would make every other node tear down its peers.
type HaReconciler struct {
	m *Manager

	lastFingerprint [32]byte
	// interfaces applied by this node, persisted in stateFile so ones deleted while it was down still go
	managed   map[domain.InterfaceIdentifier]domain.Interface
	stateFile string
}

func NewHaReconciler(m *Manager) *HaReconciler {
	r := &HaReconciler{m: m, managed: map[domain.InterfaceIdentifier]domain.Interface{},
		stateFile: filepath.Join(m.cfg.Ha.DataDir, "reconciled-interfaces.json")}
	raw, err := os.ReadFile(r.stateFile)
	if err == nil {
		err = json.Unmarshal(raw, &r.managed)
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Warn("ha reconcile: cannot read managed interfaces, deleted ones may stay in the kernel",
			"file", r.stateFile, "error", err)
	}
	return r
}

// saveManaged persists the managed interfaces without their private keys.
func (r *HaReconciler) saveManaged() {
	stripped := make(map[domain.InterfaceIdentifier]domain.Interface, len(r.managed))
	for id, iface := range r.managed {
		iface.KeyPair = domain.KeyPair{}
		stripped[id] = iface
	}
	raw, err := json.Marshal(stripped)
	if err == nil {
		err = os.WriteFile(r.stateFile, raw, 0o600)
	}
	if err != nil {
		slog.Warn("ha reconcile: cannot save managed interfaces", "file", r.stateFile, "error", err)
	}
}

// Run reconciles immediately and then every interval until ctx is cancelled.
func (r *HaReconciler) Run(ctx context.Context, interval time.Duration) {
	ctx = domain.SetUserInfo(ctx, domain.SystemAdminContextUserInfo())
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := r.Reconcile(ctx, false); err != nil {
			// a DB error (e.g. Galera non-primary) keeps the current kernel state untouched
			slog.Warn("ha reconcile failed, keeping current WireGuard state", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Reconcile applies the DB state to the kernel. Without force, nothing is done if the DB did not change.
// shortcut: manual kernel changes are only corrected after the next DB change or a restart.
func (r *HaReconciler) Reconcile(ctx context.Context, force bool) error {
	interfaces, err := r.m.db.GetAllInterfaces(ctx)
	if err != nil {
		return fmt.Errorf("load interfaces: %w", err)
	}
	peersByIface := make(map[domain.InterfaceIdentifier][]domain.Peer, len(interfaces))
	h := sha256.New()
	for _, iface := range interfaces {
		peers, err := r.m.db.GetInterfacePeers(ctx, iface.Identifier)
		if err != nil {
			return fmt.Errorf("load peers of %s: %w", iface.Identifier, err)
		}
		peersByIface[iface.Identifier] = peers
		_, _ = fmt.Fprintf(h, "%s|%d|%v;", iface.Identifier, iface.UpdatedAt.UnixNano(), iface.IsDisabled())
		for _, p := range peers {
			_, _ = fmt.Fprintf(h, "%s|%d|%v|%v;", p.Identifier, p.UpdatedAt.UnixNano(), p.IsDisabled(),
				p.ExpiresAt)
		}
	}
	var fingerprint [32]byte
	copy(fingerprint[:], h.Sum(nil))
	if !force && fingerprint == r.lastFingerprint {
		return nil
	}

	var errs []error
	seen := map[domain.InterfaceIdentifier]bool{}
	for _, iface := range interfaces {
		seen[iface.Identifier] = true
		if err := r.applyInterface(ctx, iface, peersByIface[iface.Identifier]); err != nil {
			errs = append(errs, err)
		}
	}
	for id, iface := range r.managed {
		if !seen[id] {
			if err := r.removeInterface(ctx, iface); err != nil {
				errs = append(errs, err)
			}
		}
	}
	r.saveManaged()
	if len(errs) > 0 {
		return fmt.Errorf("%d interface(s) failed, first: %w", len(errs), errs[0])
	}

	r.lastFingerprint = fingerprint
	slog.Debug("ha reconcile applied database state", "interfaces", len(interfaces))
	return nil
}

func (r *HaReconciler) applyInterface(ctx context.Context, iface domain.Interface, peers []domain.Peer) error {
	controller := r.m.wg.GetController(iface)
	_, err := controller.GetInterface(ctx, iface.Identifier)
	existed := err == nil

	if iface.IsDisabled() {
		if existed {
			return r.removeInterface(ctx, iface)
		}
		return nil
	}

	hooks, _ := controller.(WgQuickController)
	if !existed && hooks != nil {
		if err := hooks.ExecuteInterfaceHook(ctx, iface.Identifier, iface.PreUp); err != nil {
			return fmt.Errorf("pre-up hook of %s: %w", iface.Identifier, err)
		}
	}

	err = controller.SaveInterface(ctx, iface.Identifier,
		func(pi *domain.PhysicalInterface) (*domain.PhysicalInterface, error) {
			domain.MergeToPhysicalInterface(pi, &iface)
			return pi, nil
		})
	if err != nil {
		return fmt.Errorf("save interface %s: %w", iface.Identifier, err)
	}
	r.managed[iface.Identifier] = iface

	known := make(map[domain.PeerIdentifier]bool, len(peers))
	for i := range peers {
		peer := peers[i]
		known[peer.Identifier] = true
		err := controller.SavePeer(ctx, iface.Identifier, peer.Identifier,
			func(pp *domain.PhysicalPeer) (*domain.PhysicalPeer, error) {
				domain.MergeToPhysicalPeer(pp, &peer)
				return pp, nil
			})
		if err != nil {
			return fmt.Errorf("save peer %s of %s: %w", peer.Identifier, iface.Identifier, err)
		}
	}

	physicalPeers, err := controller.GetPeers(ctx, iface.Identifier)
	if err != nil {
		return fmt.Errorf("list peers of %s: %w", iface.Identifier, err)
	}
	for _, pp := range physicalPeers {
		if !known[domain.PeerIdentifier(pp.PublicKey)] {
			if err := controller.DeletePeer(ctx, iface.Identifier, domain.PeerIdentifier(pp.PublicKey)); err != nil {
				return fmt.Errorf("remove stale peer from %s: %w", iface.Identifier, err)
			}
		}
	}

	r.m.bus.Publish(app.TopicRouteUpdate, domain.RoutingTableInfo{
		Interface:  iface,
		AllowedIps: iface.GetAllowedIPs(peers),
		FwMark:     iface.FirewallMark,
		Table:      iface.GetRoutingTable(),
		TableStr:   iface.RoutingTable,
	})

	if !existed && hooks != nil {
		if err := hooks.ExecuteInterfaceHook(ctx, iface.Identifier, iface.PostUp); err != nil {
			return fmt.Errorf("post-up hook of %s: %w", iface.Identifier, err)
		}
	}
	return nil
}

func (r *HaReconciler) removeInterface(ctx context.Context, iface domain.Interface) error {
	controller := r.m.wg.GetController(iface)
	hooks, _ := controller.(WgQuickController)
	if hooks != nil {
		_ = hooks.ExecuteInterfaceHook(ctx, iface.Identifier, iface.PreDown)
	}
	if err := controller.DeleteInterface(ctx, iface.Identifier); err != nil {
		return fmt.Errorf("delete interface %s: %w", iface.Identifier, err)
	}
	r.m.bus.Publish(app.TopicRouteRemove, domain.RoutingTableInfo{
		Interface: iface,
		FwMark:    iface.FirewallMark,
		Table:     iface.GetRoutingTable(),
		TableStr:  iface.RoutingTable,
		IsDeleted: true,
	})
	if hooks != nil {
		_ = hooks.ExecuteInterfaceHook(ctx, iface.Identifier, iface.PostDown)
	}
	delete(r.managed, iface.Identifier)
	return nil
}
