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
		if in["username"] != "user" || in["subdomain"] != "vpn" {
			t.Errorf("unexpected input %v", in)
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
	err := d.SetA(context.Background(), "203.0.113.2", []string{"203.0.113.1", "203.0.113.2", "203.0.113.9"})
	var partial *PartialDnsError
	if !errors.As(err, &partial) {
		t.Fatalf("expected partial error, got %v", err)
	}
	want := []string{
		"zone/add_alias content=&ip=203.0.113.2",
		"zone/remove_record content=203.0.113.1&ip=",
		"zone/remove_record content=203.0.113.9&ip=",
	}
	if strings.Join(calls, "|") != strings.Join(want, "|") {
		t.Fatalf("calls %v, want %v", calls, want)
	}
}

func toStr(v any) string {
	s, _ := v.(string)
	return s
}
