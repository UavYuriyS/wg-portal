package routing

import (
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/h44z/wg-portal/internal/domain"
)

const (
	upstreamTimeout = 2 * time.Second
	minDnsTimeout   = 60
	maxDnsTimeout   = 24 * 60 * 60
)

// domainMatcher maps list domains to their kind; a domain in both kinds is blackholed.
type domainMatcher map[string]domain.RoutingListKind

func newDomainMatcher(block, bypass []string) domainMatcher {
	m := make(domainMatcher, len(block)+len(bypass))
	for _, d := range bypass {
		m[d] = domain.RoutingListBypass
	}
	for _, d := range block {
		m[d] = domain.RoutingListBlackhole
	}
	return m
}

// match returns the kind of name, or of its closest listed parent domain.
func (m domainMatcher) match(name string) (domain.RoutingListKind, bool) {
	name = strings.TrimSuffix(strings.ToLower(name), ".")
	for {
		if kind, ok := m[name]; ok {
			return kind, true
		}
		_, parent, found := strings.Cut(name, ".")
		if !found {
			return "", false
		}
		name = parent
	}
}

type dnsHit struct {
	Kind    domain.RoutingListKind
	Addr    netip.Addr
	Timeout uint32 // seconds
}

// extractHits returns the A/AAAA records of a DNS response whose question name or any owner name in the
// answer (CNAME chain) matches a list domain. Blackhole wins over bypass.
func extractHits(msg []byte, m domainMatcher) []dnsHit {
	if len(m) == 0 {
		return nil
	}
	var p dnsmessage.Parser
	header, err := p.Start(msg)
	if err != nil || !header.Response || header.RCode != dnsmessage.RCodeSuccess {
		return nil
	}
	questions, err := p.AllQuestions()
	if err != nil {
		return nil
	}
	var names []string
	for _, q := range questions {
		names = append(names, q.Name.String())
	}
	var hits []dnsHit
	for {
		h, err := p.AnswerHeader()
		if errors.Is(err, dnsmessage.ErrSectionDone) {
			break
		}
		if err != nil {
			return nil
		}
		names = append(names, h.Name.String())
		timeout := min(max(h.TTL, minDnsTimeout), maxDnsTimeout)
		switch h.Type {
		case dnsmessage.TypeA:
			r, err := p.AResource()
			if err != nil {
				return nil
			}
			hits = append(hits, dnsHit{Addr: netip.AddrFrom4(r.A), Timeout: timeout})
		case dnsmessage.TypeAAAA:
			r, err := p.AAAAResource()
			if err != nil {
				return nil
			}
			hits = append(hits, dnsHit{Addr: netip.AddrFrom16(r.AAAA), Timeout: timeout})
		default:
			if err := p.SkipAnswer(); err != nil {
				return nil
			}
		}
	}

	var kind domain.RoutingListKind
	for _, n := range names {
		if k, ok := m.match(n); ok && (kind == "" || k == domain.RoutingListBlackhole) {
			kind = k
		}
	}
	if kind == "" {
		return nil
	}
	for i := range hits {
		hits[i].Kind = kind
	}
	return hits
}

// DnsEntryAdder stores a learned address in the DNS set of the given kind.
type DnsEntryAdder interface {
	AddDnsEntry(kind domain.RoutingListKind, addr netip.Addr, timeout uint32) error
}

// dnsForwarder answers peers on the server interface addresses (UDP and TCP port 53), forwards queries to
// the upstreams in order and feeds matching answers into the DNS sets.
type dnsForwarder struct {
	sets      DnsEntryAdder
	matcher   *atomic.Pointer[domainMatcher]
	upstreams atomic.Pointer[[]string]

	mu        sync.Mutex
	listeners map[netip.Addr]*dnsListener
}

type dnsListener struct {
	udp net.PacketConn
	tcp net.Listener
}

func (l *dnsListener) close() {
	_ = l.udp.Close()
	_ = l.tcp.Close()
}

func newDnsForwarder(sets DnsEntryAdder, matcher *atomic.Pointer[domainMatcher]) *dnsForwarder {
	return &dnsForwarder{sets: sets, matcher: matcher, listeners: map[netip.Addr]*dnsListener{}}
}

// Sync listens on addrs and stops listening elsewhere. Addresses that cannot be bound yet (the interface
// does not exist yet) are retried on the next call.
func (f *dnsForwarder) Sync(addrs []netip.Addr, upstreams []string) {
	f.upstreams.Store(&upstreams)
	f.mu.Lock()
	defer f.mu.Unlock()

	want := map[netip.Addr]bool{}
	for _, a := range addrs {
		want[a] = true
		if _, ok := f.listeners[a]; ok {
			continue
		}
		l, err := listenDns(a)
		if err != nil {
			slog.Debug("routing: dns forwarder cannot listen yet, retrying later", "addr", a, "error", err)
			continue
		}
		f.listeners[a] = l
		go f.serveUdp(l.udp)
		go f.serveTcp(l.tcp)
		slog.Info("routing: dns forwarder listening", "addr", a)
	}
	for a, l := range f.listeners {
		if !want[a] {
			l.close()
			delete(f.listeners, a)
			slog.Info("routing: dns forwarder stopped", "addr", a)
		}
	}
}

func listenDns(a netip.Addr) (*dnsListener, error) {
	addr := netip.AddrPortFrom(a, 53).String()
	udp, err := net.ListenPacket("udp", addr)
	if err != nil {
		return nil, err
	}
	tcp, err := net.Listen("tcp", addr)
	if err != nil {
		_ = udp.Close()
		return nil, err
	}
	return &dnsListener{udp: udp, tcp: tcp}, nil
}

func (f *dnsForwarder) serveUdp(conn net.PacketConn) {
	buf := make([]byte, 65535)
	for {
		n, client, err := conn.ReadFrom(buf)
		if err != nil {
			return // closed
		}
		query := append([]byte(nil), buf[:n]...)
		// shortcut: one goroutine per query and no rate limit, peers are trusted; add a worker pool if abused
		go func() {
			if resp := f.forward(query, "udp"); resp != nil {
				_, _ = conn.WriteTo(resp, client)
			}
		}()
	}
}

func (f *dnsForwarder) serveTcp(l net.Listener) {
	for {
		conn, err := l.Accept()
		if err != nil {
			return // closed
		}
		go func() {
			defer conn.Close()
			for {
				_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
				query, err := readTcpMsg(conn)
				if err != nil {
					return
				}
				resp := f.forward(query, "tcp")
				if resp == nil || writeTcpMsg(conn, resp) != nil {
					return
				}
			}
		}()
	}
}

// forward tries the upstreams in order and returns the first answer, or nil if all failed.
func (f *dnsForwarder) forward(query []byte, network string) []byte {
	for _, upstream := range *f.upstreams.Load() {
		resp, err := exchange(query, network, upstream)
		if err != nil {
			slog.Debug("routing: dns upstream failed", "upstream", upstream, "error", err)
			continue
		}
		f.learn(resp)
		return resp
	}
	return nil
}

func (f *dnsForwarder) learn(resp []byte) {
	m := f.matcher.Load()
	if m == nil {
		return
	}
	for _, hit := range extractHits(resp, *m) {
		if err := f.sets.AddDnsEntry(hit.Kind, hit.Addr, hit.Timeout); err != nil {
			slog.Warn("routing: failed to add dns entry", "kind", hit.Kind, "addr", hit.Addr, "error", err)
		}
	}
}

func exchange(query []byte, network, upstream string) ([]byte, error) {
	conn, err := net.DialTimeout(network, upstream, upstreamTimeout)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(upstreamTimeout))
	if network == "tcp" {
		if err := writeTcpMsg(conn, query); err != nil {
			return nil, err
		}
		return readTcpMsg(conn)
	}
	if _, err := conn.Write(query); err != nil {
		return nil, err
	}
	buf := make([]byte, 65535)
	n, err := conn.Read(buf)
	if err != nil {
		return nil, err
	}
	return buf[:n], nil
}

func readTcpMsg(r io.Reader) ([]byte, error) {
	var size uint16
	if err := binary.Read(r, binary.BigEndian, &size); err != nil {
		return nil, err
	}
	msg := make([]byte, size)
	_, err := io.ReadFull(r, msg)
	return msg, err
}

func writeTcpMsg(w io.Writer, msg []byte) error {
	_, err := w.Write(append(binary.BigEndian.AppendUint16(nil, uint16(len(msg))), msg...))
	return err
}

// normalizeUpstream validates host[:port] and adds the default port 53.
func normalizeUpstream(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", false
	}
	if host, port, err := net.SplitHostPort(s); err == nil {
		p, err := net.LookupPort("udp", port)
		if host == "" || err != nil || p == 0 {
			return "", false
		}
		return net.JoinHostPort(host, port), true
	}
	if strings.ContainsAny(s, "[]/ ") {
		return "", false
	}
	return net.JoinHostPort(s, "53"), true // plain host or bare IPv6
}
