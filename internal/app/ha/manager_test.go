package ha

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm/schema"

	"github.com/h44z/wg-portal/internal/adapters"
	"github.com/h44z/wg-portal/internal/app"
	"github.com/h44z/wg-portal/internal/config"
	"github.com/h44z/wg-portal/internal/domain"
)

func init() {
	schema.RegisterSerializer("encstr", app.NewGormEncryptedStringSerializer("test-passphrase"))
}

func newTestManager(t *testing.T) (*Manager, *adapters.SqlRepo, *config.Config) {
	t.Helper()
	cfg := &config.Config{}
	cfg.Database = config.DatabaseConfig{Type: config.DatabaseSQLite, DSN: filepath.Join(t.TempDir(), "db.sqlite")}
	db, err := adapters.NewDatabase(cfg.Database)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := adapters.NewSqlRepository(db, cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Database.DSN = "wgportal:secret@tcp(mariadb:3306)/wgportal?parseTime=true" // what nodes get rewritten
	cfg.Database.EncryptionPassphrase = "test-passphrase"
	cfg.Ha = config.HaConfig{Role: config.HaRoleOrchestrator, NodeId: "orch", PublicIp: "198.51.100.1",
		DataDir: t.TempDir(), Galera: config.HaGaleraConfig{ClusterName: "test"}}
	return NewManager(cfg, repo, nil, nil), repo, cfg
}

func TestRegister(t *testing.T) {
	m, repo, cfg := newTestManager(t)
	ctx := domain.SetUserInfo(context.Background(), domain.SystemAdminContextUserInfo())

	caCert, caKey, err := NewCa("test")
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveHaState(ctx, func(s *domain.HaState) error {
		s.CaCertPem, s.CaKeyPem = caCert, caKey
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	token, err := m.CreateJoinToken(ctx, time.Hour, 1)
	if err != nil {
		t.Fatal(err)
	}

	req := RegisterRequest{NodeId: "node-a", PublicIp: "203.0.113.10", Priority: 5}
	if _, err := m.Register(ctx, "wrong", req); !errors.Is(err, ErrInvalidJoinToken) {
		t.Fatalf("wrong token: got %v", err)
	}
	// an orchestrator misconfiguration must not use up the token
	dsn := cfg.Database.DSN
	cfg.Database.DSN = "not a dsn"
	if _, err := m.Register(ctx, token, req); err == nil || errors.Is(err, ErrInvalidJoinToken) {
		t.Fatalf("expected dsn error, got %v", err)
	}
	cfg.Database.DSN = dsn
	resp, err := m.Register(ctx, token, req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Register(ctx, token, req); !errors.Is(err, ErrInvalidJoinToken) {
		t.Fatalf("used up token: got %v", err)
	}

	// certificate is signed by the cluster CA, the CA key is not leaked
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM([]byte(resp.CaCert))
	block, _ := pem.Decode([]byte(resp.Cert))
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cert.Verify(x509.VerifyOptions{Roots: pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Fatalf("node cert does not verify: %v", err)
	}
	raw, _ := json.Marshal(resp)
	if strings.Contains(string(raw), "PRIVATE KEY") && strings.Contains(string(raw), strings.TrimSpace(caKey)) {
		t.Fatal("response leaks the CA key")
	}

	if resp.DatabaseDsn != "wgportal:secret@tcp(127.0.0.1:3306)/wgportal?parseTime=true" {
		t.Fatalf("unexpected dsn %q", resp.DatabaseDsn)
	}
	if strings.Join(resp.ClusterAddresses, ",") != "198.51.100.1,203.0.113.10" {
		t.Fatalf("unexpected members %v", resp.ClusterAddresses)
	}
	members, _ := os.ReadFile(membersFile(cfg.Ha.DataDir))
	if strings.Fields(string(members))[1] != "203.0.113.10" {
		t.Fatalf("members file not updated: %q", members)
	}
	if cnf, _ := os.ReadFile(filepath.Join(cfg.Ha.DataDir, "galera.cnf")); !strings.Contains(string(cnf),
		"wsrep_cluster_address=gcomm://203.0.113.10\n") {
		t.Fatalf("galera.cnf not updated: %q", cnf)
	}

	// CA key survives the encrypted round trip
	state, _ := repo.GetHaState(ctx)
	if state.CaKeyPem != caKey {
		t.Fatal("CA key not stored/decrypted correctly")
	}

	// expired token
	expired, _ := m.CreateJoinToken(ctx, -time.Minute, 1)
	if _, err := m.Register(ctx, expired, req); !errors.Is(err, ErrInvalidJoinToken) {
		t.Fatalf("expired token: got %v", err)
	}
}

type fakeDns struct {
	ip     string
	oldIps []string
	err    error
}

func (f *fakeDns) SetA(_ context.Context, ip string, oldIps []string) error {
	f.ip, f.oldIps = ip, oldIps
	return f.err
}

func TestNodeRefreshesMembers(t *testing.T) {
	m, repo, cfg := newTestManager(t)
	cfg.Ha.Role, cfg.Ha.NodeId, cfg.Ha.PublicIp = config.HaRoleNode, "a", "203.0.113.1"
	ctx := domain.SetUserInfo(context.Background(), domain.SystemAdminContextUserInfo())
	// node a joined when only the orchestrator existed; node b joined later
	if err := writeMembers(cfg.Ha.DataDir, []string{"198.51.100.1", "203.0.113.1"}); err != nil {
		t.Fatal(err)
	}
	for _, n := range []domain.HaNode{{NodeId: "a", PublicIp: "203.0.113.1"}, {NodeId: "b", PublicIp: "203.0.113.2"}} {
		if err := repo.SaveHaNode(ctx, &n); err != nil {
			t.Fatal(err)
		}
	}

	m.heartbeat(ctx)

	if members, _ := readMembers(cfg.Ha.DataDir); strings.Join(members, ",") != "198.51.100.1,203.0.113.1,203.0.113.2" {
		t.Fatalf("members not refreshed: %v", members)
	}
	cnf, _ := os.ReadFile(filepath.Join(cfg.Ha.DataDir, "galera.cnf"))
	if !strings.Contains(string(cnf), "wsrep_cluster_address=gcomm://198.51.100.1,203.0.113.2\n") {
		t.Fatalf("galera.cnf not refreshed: %q", cnf)
	}
}

func TestPromote(t *testing.T) {
	m, repo, _ := newTestManager(t)
	ctx := domain.SetUserInfo(context.Background(), domain.SystemAdminContextUserInfo())
	for _, n := range []domain.HaNode{{NodeId: "a", PublicIp: "203.0.113.1"}, {NodeId: "b", PublicIp: "203.0.113.2"}} {
		if err := repo.SaveHaNode(ctx, &n); err != nil {
			t.Fatal(err)
		}
	}

	dns := &fakeDns{err: errors.New("api down")}
	m.dns = dns
	if err := m.Promote(ctx, "b", "test"); err == nil {
		t.Fatal("expected dns error")
	}
	if s, _ := repo.GetHaState(ctx); s.ActiveNodeId != "" {
		t.Fatal("state must not change when DNS fails")
	}

	dns.err = nil
	if err := m.Promote(ctx, "b", "test"); err != nil {
		t.Fatal(err)
	}
	if dns.ip != "203.0.113.2" || len(dns.oldIps) != 1 || dns.oldIps[0] != "203.0.113.1" {
		t.Fatalf("unexpected dns call %+v", dns)
	}
	if s, _ := repo.GetHaState(ctx); s.ActiveNodeId != "b" || s.ChangedBy != "test" {
		t.Fatalf("unexpected state %+v", s)
	}
	if err := m.RemoveNode(ctx, "b"); !errors.Is(err, domain.ErrInvalidData) {
		t.Fatalf("removing the active node must fail, got %v", err)
	}
}

func TestRegRuDns(t *testing.T) {
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("input_format") != "json" {
			t.Errorf("missing input_format")
		}
		var in map[string]any
		_ = json.Unmarshal([]byte(r.Form.Get("input_data")), &in)
		calls = append(calls, strings.TrimPrefix(r.URL.Path, "/api/regru2/")+" "+
			url.Values{"ip": {toStr(in["ipaddr"])}, "content": {toStr(in["content"])}}.Encode())
		listing := strings.HasSuffix(r.URL.Path, "get_resource_records")
		if in["username"] != "user" || (in["subdomain"] != "vpn" && !listing) {
			t.Errorf("unexpected input %v", in)
		}
		if listing {
			_, _ = w.Write([]byte(`{"result":"success","answer":{"domains":[{"dname":"example.com",` +
				`"result":"success",` +
				`"rrs":[{"subname":"vpn","rectype":"A","content":"203.0.113.1"},` +
				`{"subname":"vpn","rectype":"A","content":"203.0.113.9"},` +
				`{"subname":"www","rectype":"A","content":"203.0.113.3"}]}]}}`))
			return
		}
		if strings.HasSuffix(r.URL.Path, "remove_record") && in["content"] == "203.0.113.9" {
			_, _ = w.Write([]byte(`{"result":"success","answer":{"domains":[{"dname":"example.com","result":"error","error_code":"NO_RECORD"}]}}`))
			return
		}
		_, _ = w.Write([]byte(`{"result":"success","answer":{"domains":[{"dname":"example.com","result":"success"}]}}`))
	}))
	defer srv.Close()

	d := NewRegRuDns(config.HaRegRuConfig{ApiUrl: srv.URL + "/api/regru2/", Username: "user", Password: "pw",
		Zone: "example.com", Subdomain: "vpn"})
	err := d.SetA(context.Background(), "203.0.113.2",
		[]string{"203.0.113.1", "203.0.113.2", "203.0.113.9", "203.0.113.3"})
	var partial *PartialDnsError
	if !errors.As(err, &partial) {
		t.Fatalf("expected partial error, got %v", err)
	}
	want := []string{
		"zone/get_resource_records content=&ip=",
		"zone/add_alias content=&ip=203.0.113.2",
		"zone/remove_record content=203.0.113.1&ip=",
		"zone/remove_record content=203.0.113.9&ip=",
		"zone/remove_record content=203.0.113.3&ip=",
	}
	if strings.Join(calls, "|") != strings.Join(want, "|") {
		t.Fatalf("calls %v, want %v", calls, want)
	}

	// fail-back to an IP that still has its record: nothing to add, old records are still removed
	calls = nil
	if err := d.SetA(context.Background(), "203.0.113.1", []string{"203.0.113.2"}); err != nil {
		t.Fatal(err)
	}
	want = []string{"zone/get_resource_records content=&ip=", "zone/remove_record content=203.0.113.2&ip="}
	if strings.Join(calls, "|") != strings.Join(want, "|") {
		t.Fatalf("calls %v", calls)
	}
}

func toStr(v any) string {
	s, _ := v.(string)
	return s
}

func TestGetStatus_ConnectedPeers(t *testing.T) {
	m, repo, cfg := newTestManager(t)
	ctx := domain.SetUserInfo(context.Background(), domain.SystemAdminContextUserInfo())
	cfg.Statistics.DataCollectionInterval = time.Minute
	cfg.Backend.ReKeyTimeoutInterval = 3 * time.Minute
	now := time.Now()
	old := now.Add(-time.Hour)
	if err := repo.SaveHaProbePeer(ctx, &domain.HaProbePeer{InterfaceIdentifier: "wg0", PeerIdentifier: "probe",
		ExitPeerIdentifier: "exit-probe"}); err != nil {
		t.Fatal(err)
	}
	for node, peers := range map[string][]domain.PeerIdentifier{"a": {"p1", "probe", "exit-probe"}, "b": {"p2"}} {
		var sessions []domain.HaPeerSession
		for _, p := range peers {
			sessions = append(sessions, domain.HaPeerSession{NodeId: node, PeerIdentifier: p, LastHandshake: &now,
				UpdatedAt: now})
		}
		if err := repo.ReplaceHaPeerSessions(ctx, node, sessions); err != nil {
			t.Fatal(err)
		}
	}
	// node c stopped reporting
	if err := repo.ReplaceHaPeerSessions(ctx, "c", []domain.HaPeerSession{
		{NodeId: "c", PeerIdentifier: "p3", LastHandshake: &old, UpdatedAt: old}}); err != nil {
		t.Fatal(err)
	}

	s, err := m.GetStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if s.ConnectedPeers["a"] != 1 || s.ConnectedPeers["b"] != 1 || s.ConnectedPeers["c"] != 0 {
		t.Fatalf("unexpected connected peers %v", s.ConnectedPeers)
	}

	if err := m.RemoveNode(ctx, "b"); err != nil {
		t.Fatal(err)
	}
	if left, _ := repo.GetHaPeerSessions(ctx, "b"); len(left) != 0 {
		t.Fatalf("sessions of the removed node must be deleted, got %v", left)
	}
}
