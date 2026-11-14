package ha

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"

	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun/netstack"

	"github.com/h44z/wg-portal/internal/domain"
)

// ProbeTarget describes how to reach one node with the probe peer.
type ProbeTarget struct {
	Endpoint        string // node public ip:port, never the DNS name
	ServerPublicKey string
	ClientKeys      domain.KeyPair
	PresharedKey    domain.PreSharedKey
	ClientAddress   netip.Addr
	ProbeUrl        string // returns the caller IP as plain text
}

// Prober returns the egress IP that ProbeUrl observed when requested through the node.
type Prober func(ctx context.Context, t ProbeTarget) (string, error)

// WireGuardProbe connects to the node with an in-process (userspace) WireGuard client, so the
// orchestrator needs neither root nor a kernel interface, and requests ProbeUrl through the tunnel.
func WireGuardProbe(ctx context.Context, t ProbeTarget) (string, error) {
	tunDev, tnet, err := netstack.CreateNetTUN([]netip.Addr{t.ClientAddress}, nil, 1420)
	if err != nil {
		return "", fmt.Errorf("create netstack: %w", err)
	}
	dev := device.NewDevice(tunDev, conn.NewDefaultBind(), device.NewLogger(device.LogLevelSilent, ""))
	defer dev.Close()

	uapi, err := probeUapiConfig(t)
	if err != nil {
		return "", err
	}
	if err := dev.IpcSet(uapi); err != nil {
		return "", fmt.Errorf("configure probe device: %w", err)
	}
	if err := dev.Up(); err != nil {
		return "", fmt.Errorf("start probe device: %w", err)
	}

	client := &http.Client{Transport: &http.Transport{
		// resolve on the orchestrator, but connect through the tunnel
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, portStr, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			port, err := strconv.ParseUint(portStr, 10, 16)
			if err != nil {
				return nil, err
			}
			ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip4", host)
			if err != nil || len(ips) == 0 {
				return nil, fmt.Errorf("resolve %s: %w", host, err)
			}
			// the resolver returns IPv4-mapped IPv6 addresses, netstack needs plain IPv4
			return tnet.DialContextTCPAddrPort(ctx, netip.AddrPortFrom(ips[0].Unmap(), uint16(port)))
		},
		DisableKeepAlives: true,
	}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.ProbeUrl, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("request through tunnel: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("probe url returned %s", resp.Status)
	}
	ip := strings.TrimSpace(string(body))
	if _, err := netip.ParseAddr(ip); err != nil {
		return "", fmt.Errorf("probe url returned no ip: %q", ip)
	}
	return ip, nil
}

func probeUapiConfig(t ProbeTarget) (string, error) {
	toHex := func(b64 string) (string, error) {
		raw, err := base64.StdEncoding.DecodeString(b64)
		if err != nil || len(raw) != 32 {
			return "", fmt.Errorf("invalid wireguard key")
		}
		return hex.EncodeToString(raw), nil
	}
	priv, err := toHex(t.ClientKeys.PrivateKey)
	if err != nil {
		return "", fmt.Errorf("probe private key: %w", err)
	}
	pub, err := toHex(t.ServerPublicKey)
	if err != nil {
		return "", fmt.Errorf("server public key: %w", err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "private_key=%s\npublic_key=%s\nendpoint=%s\nallowed_ip=0.0.0.0/0\n", priv, pub, t.Endpoint)
	if t.PresharedKey != "" {
		psk, err := toHex(string(t.PresharedKey))
		if err != nil {
			return "", fmt.Errorf("preshared key: %w", err)
		}
		fmt.Fprintf(&b, "preshared_key=%s\n", psk)
	}
	return b.String(), nil
}
