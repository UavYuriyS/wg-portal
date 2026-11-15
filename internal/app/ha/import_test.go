package ha

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/h44z/wg-portal/internal/adapters"
	"github.com/h44z/wg-portal/internal/app"
	"github.com/h44z/wg-portal/internal/config"
	"github.com/h44z/wg-portal/internal/domain"
)

func TestImportRejectsForeignPassphrase(t *testing.T) {
	ctx := domain.SetUserInfo(context.Background(), domain.SystemAdminContextUserInfo())
	cfg := &config.Config{}
	srcFile := filepath.Join(t.TempDir(), "sqlite.db")
	srcDb, err := adapters.NewDatabase(config.DatabaseConfig{Type: config.DatabaseSQLite, DSN: srcFile})
	if err != nil {
		t.Fatal(err)
	}
	src, err := adapters.NewSqlRepository(srcDb, cfg)
	if err != nil {
		t.Fatal(err)
	}
	err = src.SaveUser(ctx, "alice", func(u *domain.User) (*domain.User, error) {
		u.Identifier, u.Email = "alice", "alice@example.com"
		return u, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// the old installation encrypted with another passphrase than the cluster's ("test-passphrase")
	foreign, _ := app.EncryptAES256("api-token-0123456789", "passphrase-of-the-old-installation")
	if err := srcDb.Exec("UPDATE users SET api_token = ?", "WG_ENC_"+foreign).Error; err != nil {
		t.Fatal(err)
	}
	if sql, err := srcDb.DB(); err == nil {
		_ = sql.Close()
	}

	dstDb, err := adapters.NewDatabase(config.DatabaseConfig{Type: config.DatabaseSQLite,
		DSN: filepath.Join(t.TempDir(), "cluster.db")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapters.NewSqlRepository(dstDb, cfg); err != nil {
		t.Fatal(err)
	}
	err = ImportStandalone(cfg, dstDb, srcFile)
	if err == nil || !strings.Contains(err.Error(), "does not decrypt") {
		t.Fatalf("import must refuse a foreign passphrase, got %v", err)
	}
	var users int64
	if dstDb.Table("users").Count(&users); users != 0 {
		t.Fatalf("failed import must roll back, %d users left", users)
	}
}

func TestImportStandalone(t *testing.T) {
	ctx := domain.SetUserInfo(context.Background(), domain.SystemAdminContextUserInfo())
	cfg := &config.Config{}

	// standalone source database with an interface, a peer (encrypted keys, many2many addresses) and a user
	srcFile := filepath.Join(t.TempDir(), "sqlite.db")
	srcDb, err := adapters.NewDatabase(config.DatabaseConfig{Type: config.DatabaseSQLite, DSN: srcFile})
	if err != nil {
		t.Fatal(err)
	}
	src, err := adapters.NewSqlRepository(srcDb, cfg)
	if err != nil {
		t.Fatal(err)
	}
	ifaceKeys, _ := domain.NewFreshKeypair()
	peerKeys, _ := domain.NewFreshKeypair()
	ifaceAddrs, _ := domain.CidrsFromString("10.11.12.1/24")
	peerAddrs, _ := domain.CidrsFromString("10.11.12.2/32")
	err = src.SaveInterface(ctx, "wg0", func(in *domain.Interface) (*domain.Interface, error) {
		in.Identifier, in.KeyPair, in.ListenPort, in.Addresses = "wg0", ifaceKeys, 51820, ifaceAddrs
		in.Type = domain.InterfaceTypeServer
		return in, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	peerId := domain.PeerIdentifier(peerKeys.PublicKey)
	err = src.SavePeer(ctx, peerId, func(p *domain.Peer) (*domain.Peer, error) {
		p.Identifier, p.InterfaceIdentifier, p.DisplayName = peerId, "wg0", "laptop"
		p.Interface.KeyPair, p.Interface.Addresses = peerKeys, peerAddrs
		p.PresharedKey = "psk-value"
		return p, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	err = src.SaveUser(ctx, "alice", func(u *domain.User) (*domain.User, error) {
		u.Identifier, u.Email, u.IsAdmin = "alice", "alice@example.com", true
		return u, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// an install without passphrase stored these in plain text; the peer's private key stays encrypted
	plainCols := map[string]string{"interfaces": "private_key", "peers": "preshared_key"}
	if err := srcDb.Exec("UPDATE interfaces SET private_key = ?", ifaceKeys.PrivateKey).Error; err != nil {
		t.Fatal(err)
	}
	if err := srcDb.Exec("UPDATE peers SET preshared_key = ?", "psk-value").Error; err != nil {
		t.Fatal(err)
	}
	if sql, err := srcDb.DB(); err == nil {
		_ = sql.Close() // like a stopped standalone instance
	}

	// fresh cluster database
	dstDb, err := adapters.NewDatabase(config.DatabaseConfig{Type: config.DatabaseSQLite,
		DSN: filepath.Join(t.TempDir(), "cluster.db")})
	if err != nil {
		t.Fatal(err)
	}
	dst, err := adapters.NewSqlRepository(dstDb, cfg)
	if err != nil {
		t.Fatal(err)
	}

	if err := ImportStandalone(cfg, dstDb, srcFile); err != nil {
		t.Fatal(err)
	}

	for table, col := range plainCols { // re-encrypted with the target's passphrase
		var raw string
		if err := dstDb.Table(table).Select(col).Scan(&raw).Error; err != nil || !strings.HasPrefix(raw, "WG_ENC_") {
			t.Fatalf("%s.%s not encrypted after import: %q %v", table, col, raw, err)
		}
	}

	iface, peers, err := dst.GetInterfaceAndPeers(ctx, "wg0")
	if err != nil {
		t.Fatal(err)
	}
	if iface.PrivateKey != ifaceKeys.PrivateKey || iface.ListenPort != 51820 || len(iface.Addresses) != 1 ||
		iface.Addresses[0].Cidr != "10.11.12.1/24" {
		t.Fatalf("interface not imported correctly: %+v", iface)
	}
	if len(peers) != 1 || peers[0].Interface.PrivateKey != peerKeys.PrivateKey || peers[0].PresharedKey != "psk-value" ||
		len(peers[0].Interface.Addresses) != 1 || peers[0].Interface.Addresses[0].Cidr != "10.11.12.2/32" {
		t.Fatalf("peer not imported correctly: %+v", peers)
	}
	if u, err := dst.GetUser(ctx, "alice"); err != nil || !u.IsAdmin {
		t.Fatalf("user not imported: %v %+v", err, u)
	}

	// importing twice must not duplicate or overwrite anything
	err = ImportStandalone(cfg, dstDb, srcFile)
	if err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("second import must be refused, got %v", err)
	}
}
