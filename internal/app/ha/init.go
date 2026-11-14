package ha

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/h44z/wg-portal/internal/config"
)

// Files below ha.data_dir. The directory is mounted into the MariaDB container as /etc/mysql/ha.
const (
	galeraMountDir = "/etc/mysql/ha"
	joinedMarker   = "joined"
	// mysqlGid is the group of the mysql user in the official MariaDB image; it must read certs and keys.
	mysqlGid = 999
)

func caCertFile(dir string) string { return filepath.Join(dir, "ca.pem") }
func caKeyFile(dir string) string  { return filepath.Join(dir, "ca-key.pem") }
func membersFile(dir string) string {
	return filepath.Join(dir, "members")
}

// Init prepares this party before MariaDB starts ("wg-portal -ha-init"):
//   - orchestrator: creates the cluster CA and its own certificate on first run.
//   - node: registers at the orchestrator with the join token on first run.
//
// On every run it regenerates galera.cnf from the known cluster members.
func Init(ctx context.Context, cfg *config.Config) error {
	dir := cfg.Ha.DataDir
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	_ = os.Chown(dir, -1, mysqlGid) // best effort: only works when running as root (container)
	switch cfg.Ha.Role {
	case config.HaRoleOrchestrator:
		if err := initOrchestrator(cfg); err != nil {
			return err
		}
	case config.HaRoleNode:
		if err := initNode(ctx, cfg); err != nil {
			return err
		}
	default:
		return fmt.Errorf("ha.role must be orchestrator or node for -ha-init")
	}

	members, err := readMembers(dir)
	if err != nil {
		return err
	}
	return writeGaleraConfig(cfg, members)
}

func initOrchestrator(cfg *config.Config) error {
	dir := cfg.Ha.DataDir
	if _, err := os.Stat(caKeyFile(dir)); errors.Is(err, os.ErrNotExist) {
		slog.Info("ha: creating cluster CA", "dir", dir)
		certPem, keyPem, err := NewCa(cfg.Ha.Galera.ClusterName)
		if err != nil {
			return err
		}
		if err := writeFiles(map[string]string{caCertFile(dir): certPem, caKeyFile(dir): keyPem}); err != nil {
			return err
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "cert.pem")); errors.Is(err, os.ErrNotExist) {
		certPem, err := os.ReadFile(caCertFile(dir))
		if err != nil {
			return err
		}
		keyPem, err := os.ReadFile(caKeyFile(dir))
		if err != nil {
			return err
		}
		cert, key, err := SignMemberCert(string(certPem), string(keyPem), cfg.Ha.NodeId, cfg.Ha.PublicIp)
		if err != nil {
			return err
		}
		if err := writeFiles(map[string]string{
			filepath.Join(dir, "cert.pem"): cert, filepath.Join(dir, "key.pem"): key,
		}); err != nil {
			return err
		}
	}
	return nil
}

func initNode(ctx context.Context, cfg *config.Config) error {
	dir := cfg.Ha.DataDir
	if _, err := os.Stat(filepath.Join(dir, joinedMarker)); err == nil {
		return nil // already registered, only galera.cnf is refreshed
	}
	if cfg.Ha.OrchestratorUrl == "" || cfg.Ha.JoinToken == "" {
		return fmt.Errorf("ha.orchestrator_url and ha.join_token are required to join the cluster")
	}

	publicIp := cfg.Ha.PublicIp
	if publicIp == "" {
		ip, err := detectPublicIp(ctx, cfg.Ha.ProbeUrl)
		if err != nil {
			return fmt.Errorf("detect public ip (set ha.public_ip to skip): %w", err)
		}
		publicIp = ip
	}

	body, _ := json.Marshal(RegisterRequest{NodeId: cfg.Ha.NodeId, PublicIp: publicIp, Priority: cfg.Ha.Priority})
	url := strings.TrimSuffix(cfg.Ha.OrchestratorUrl, "/") + "/api/v1/ha/register"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.Ha.JoinToken)
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return fmt.Errorf("register at %s: %w", url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("register at %s: %s: %s", url, resp.Status, strings.TrimSpace(string(raw)))
	}
	var r RegisterResponse
	if err := json.Unmarshal(raw, &r); err != nil {
		return fmt.Errorf("invalid register response: %w", err)
	}

	nodeCfg := map[string]any{
		"database": map[string]any{
			"type":                  "mysql",
			"dsn":                   r.DatabaseDsn,
			"encryption_passphrase": r.EncryptionPassphrase,
		},
		"web": map[string]any{"listening_address": "127.0.0.1:8888"},
		"ha": map[string]any{
			"role":      "node",
			"node_id":   r.NodeId,
			"public_ip": publicIp,
			"priority":  cfg.Ha.Priority,
			"data_dir":  dir,
			"galera": map[string]any{
				"cluster_name": r.ClusterName,
				"sst_user":     r.SstUser,
				"sst_password": r.SstPassword,
			},
		},
	}
	cfgYaml, err := yaml.Marshal(nodeCfg)
	if err != nil {
		return err
	}
	err = writeFiles(map[string]string{
		caCertFile(dir):                    r.CaCert,
		filepath.Join(dir, "cert.pem"):     r.Cert,
		filepath.Join(dir, "key.pem"):      r.Key,
		filepath.Join(dir, "config.yml"):   string(cfgYaml),
		filepath.Join(dir, "listen-ports"): joinInts(r.ListenPorts),
	})
	if err != nil {
		return err
	}
	if err := writeMembers(dir, r.ClusterAddresses); err != nil {
		return err
	}
	// galera.cnf needs the sst credentials and cluster name from the response
	cfg.Ha.Galera = config.HaGaleraConfig{ClusterName: r.ClusterName, SstUser: r.SstUser, SstPassword: r.SstPassword}
	cfg.Ha.PublicIp = publicIp
	cfg.Ha.NodeId = r.NodeId
	if err := os.WriteFile(filepath.Join(dir, joinedMarker), []byte(time.Now().Format(time.RFC3339)), 0o600); err != nil {
		return err
	}
	slog.Info("ha: node registered", "node", r.NodeId, "ip", publicIp, "members", r.ClusterAddresses)
	return nil
}

// writeGaleraConfig renders galera.cnf. Without known members the orchestrator bootstraps a new cluster.
func writeGaleraConfig(cfg *config.Config, members []string) error {
	if cfg.Ha.IsNode() {
		if nodeCfg, err := loadGeneratedNodeConfig(cfg.Ha.DataDir); err == nil {
			cfg.Ha.Galera, cfg.Ha.PublicIp, cfg.Ha.NodeId = nodeCfg.Galera, nodeCfg.PublicIp, nodeCfg.NodeId
		}
	}
	var others []string
	for _, m := range members {
		if m != "" && m != cfg.Ha.PublicIp {
			others = append(others, m)
		}
	}
	tls := fmt.Sprintf("%[1]s/cert.pem;socket.ssl_key=%[1]s/key.pem;socket.ssl_ca=%[1]s/ca.pem", galeraMountDir)
	cnf := fmt.Sprintf(`# generated by wg-portal -ha-init, do not edit
[mysqld]
bind-address=127.0.0.1
binlog_format=ROW
default_storage_engine=InnoDB
innodb_autoinc_lock_mode=2
wsrep_on=ON
wsrep_provider=/usr/lib/galera/libgalera_smm.so
wsrep_cluster_name=%s
wsrep_cluster_address=gcomm://%s
wsrep_node_name=%s
wsrep_node_address=%s
wsrep_sst_method=mariabackup
wsrep_sst_auth=%s:%s
wsrep_provider_options="socket.ssl_cert=%s"
ssl_cert=%[8]s/cert.pem
ssl_key=%[8]s/key.pem
ssl_ca=%[8]s/ca.pem

[sst]
ssl-mode=VERIFY_CA
`, cfg.Ha.Galera.ClusterName, strings.Join(others, ","), cfg.Ha.NodeId, cfg.Ha.PublicIp,
		cfg.Ha.Galera.SstUser, cfg.Ha.Galera.SstPassword, tls, galeraMountDir)
	return writeFiles(map[string]string{filepath.Join(cfg.Ha.DataDir, "galera.cnf"): cnf})
}

func loadGeneratedNodeConfig(dir string) (config.HaConfig, error) {
	var c struct {
		Ha config.HaConfig `yaml:"ha"`
	}
	raw, err := os.ReadFile(filepath.Join(dir, "config.yml"))
	if err != nil {
		return c.Ha, err
	}
	err = yaml.Unmarshal(raw, &c)
	return c.Ha, err
}

// ExportCa writes the cluster CA from the (replicated) database into dir. Used to rebuild a lost
// orchestrator from any node: "wg-portal -ha-export-ca <dir>".
func ExportCa(ctx context.Context, repo Repo, dir string) error {
	state, err := repo.GetHaState(ctx)
	if err != nil {
		return err
	}
	if state.CaKeyPem == "" {
		return errors.New("no cluster CA stored in the database")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return writeFiles(map[string]string{caCertFile(dir): state.CaCertPem, caKeyFile(dir): state.CaKeyPem})
}

func readMembers(dir string) ([]string, error) {
	raw, err := os.ReadFile(membersFile(dir))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return strings.Fields(string(raw)), nil
}

func writeMembers(dir string, members []string) error {
	return writeFiles(map[string]string{membersFile(dir): strings.Join(members, "\n") + "\n"})
}

// writeFiles writes files with mode 0640 and group mysqlGid, so MariaDB in its container can read certs
// and keys while other users cannot.
func writeFiles(files map[string]string) error {
	for name, content := range files {
		if err := os.WriteFile(name, []byte(content), 0o640); err != nil {
			return fmt.Errorf("write %s: %w", name, err)
		}
		_ = os.Chown(name, -1, mysqlGid) // best effort: only works when running as root (container)
	}
	return nil
}

func joinInts(v []int) string {
	s := make([]string, len(v))
	for i, n := range v {
		s[i] = fmt.Sprint(n)
	}
	return strings.Join(s, "\n") + "\n"
}

func detectPublicIp(ctx context.Context, probeUrl string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, probeUrl, nil)
	if err != nil {
		return "", err
	}
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 256))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(raw)), nil
}
