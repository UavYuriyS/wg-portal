package routing

import (
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/dns/dnsmessage"

	"github.com/h44z/wg-portal/internal/domain"
)

type testRR struct {
	name  string
	ttl   uint32
	cname string
	addr  string
}

func buildResponse(t *testing.T, question string, rcode dnsmessage.RCode, answers ...testRR) []byte {
	t.Helper()
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: 7, Response: true, RCode: rcode})
	require.NoError(t, b.StartQuestions())
	require.NoError(t, b.Question(dnsmessage.Question{Name: dnsmessage.MustNewName(question),
		Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}))
	require.NoError(t, b.StartAnswers())
	for _, a := range answers {
		h := dnsmessage.ResourceHeader{Name: dnsmessage.MustNewName(a.name), Class: dnsmessage.ClassINET, TTL: a.ttl}
		switch {
		case a.cname != "":
			require.NoError(t, b.CNAMEResource(h, dnsmessage.CNAMEResource{CNAME: dnsmessage.MustNewName(a.cname)}))
		case netip.MustParseAddr(a.addr).Is4():
			require.NoError(t, b.AResource(h, dnsmessage.AResource{A: netip.MustParseAddr(a.addr).As4()}))
		default:
			require.NoError(t, b.AAAAResource(h, dnsmessage.AAAAResource{AAAA: netip.MustParseAddr(a.addr).As16()}))
		}
	}
	msg, err := b.Finish()
	require.NoError(t, err)
	return msg
}

func TestDomainMatcher(t *testing.T) {
	m := newDomainMatcher([]string{"blocked.test", "both.test"}, []string{"example.com", "both.test"})
	for name, want := range map[string]domain.RoutingListKind{
		"example.com.":         domain.RoutingListBypass,
		"WWW.Example.com.":     domain.RoutingListBypass, // subdomain, case insensitive
		"a.b.blocked.test":     domain.RoutingListBlackhole,
		"both.test.":           domain.RoutingListBlackhole, // blackhole wins
		"notexample.com.":      "",                          // suffix match is label based
		"example.com.evil.net": "",
	} {
		got, _ := m.match(name)
		assert.Equal(t, want, got, name)
	}
}

func TestExtractHits(t *testing.T) {
	m := newDomainMatcher([]string{"blocked.test"}, []string{"video.example", "cdn.example"})

	// direct match, TTL clamped to the 60s minimum
	hits := extractHits(buildResponse(t, "www.video.example.", dnsmessage.RCodeSuccess,
		testRR{name: "www.video.example.", ttl: 5, addr: "192.0.2.1"},
		testRR{name: "www.video.example.", ttl: 300, addr: "2001:db8::1"}), m)
	assert.Equal(t, []dnsHit{
		{Kind: domain.RoutingListBypass, Addr: netip.MustParseAddr("192.0.2.1"), Timeout: 60},
		{Kind: domain.RoutingListBypass, Addr: netip.MustParseAddr("2001:db8::1"), Timeout: 300},
	}, hits)

	// the question does not match, but a CNAME owner in the chain does; TTL clamped to 1 day
	hits = extractHits(buildResponse(t, "unlisted.test.", dnsmessage.RCodeSuccess,
		testRR{name: "unlisted.test.", ttl: 60, cname: "edge.cdn.example."},
		testRR{name: "edge.cdn.example.", ttl: 60, cname: "host.provider.net."},
		testRR{name: "host.provider.net.", ttl: 999999, addr: "198.51.100.7"}), m)
	assert.Equal(t, []dnsHit{
		{Kind: domain.RoutingListBypass, Addr: netip.MustParseAddr("198.51.100.7"), Timeout: 24 * 60 * 60},
	}, hits)

	// a chain through a bypassed and a blackholed name is blackholed
	hits = extractHits(buildResponse(t, "www.video.example.", dnsmessage.RCodeSuccess,
		testRR{name: "www.video.example.", ttl: 60, cname: "x.blocked.test."},
		testRR{name: "x.blocked.test.", ttl: 120, addr: "203.0.113.5"}), m)
	require.Len(t, hits, 1)
	assert.Equal(t, domain.RoutingListBlackhole, hits[0].Kind)

	// no match, NXDOMAIN and garbage yield nothing
	assert.Empty(t, extractHits(buildResponse(t, "other.test.", dnsmessage.RCodeSuccess,
		testRR{name: "other.test.", ttl: 60, addr: "192.0.2.2"}), m))
	assert.Empty(t, extractHits(buildResponse(t, "www.video.example.", dnsmessage.RCodeNameError), m))
	assert.Empty(t, extractHits([]byte{1, 2, 3}, m))
}

func TestNormalizeUpstream(t *testing.T) {
	for in, want := range map[string]string{
		"192.0.2.53":       "192.0.2.53:53",
		" 192.0.2.53:5353": "192.0.2.53:5353",
		"dns.example":      "dns.example:53",
		"2001:db8::53":     "[2001:db8::53]:53",
		"[2001:db8::53]:5": "[2001:db8::53]:5",
		"":                 "",
		"host:notaport":    "",
	} {
		got, _ := normalizeUpstream(in)
		assert.Equal(t, want, got, in)
	}
}
