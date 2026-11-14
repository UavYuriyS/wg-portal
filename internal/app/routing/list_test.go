package routing

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/h44z/wg-portal/internal/domain"
)

func TestParseList(t *testing.T) {
	list, err := ParseList(`# comment
192.0.2.1
198.51.100.0/24   # trailing comment
10.1.2.3/8
2001:db8::1
2001:db8:1::/48
::ffff:192.0.2.9
Example.COM
*.ads.example.net
.tracker.example.org
cdn.example.com.

0.0.0.0 hosts-a.example.com hosts-b.example.com
127.0.0.1 hosts-c.example.com
:: hosts-d.example.com
127.0.0.1 localhost
::1 ip6-localhost
255.255.255.255 broadcasthost
fe80::1%lo0 localhost
0.0.0.0 0.0.0.0
`)
	require.NoError(t, err)
	assert.Equal(t, []netip.Prefix{
		netip.MustParsePrefix("192.0.2.1/32"),
		netip.MustParsePrefix("198.51.100.0/24"),
		netip.MustParsePrefix("10.0.0.0/8"), // masked
		netip.MustParsePrefix("2001:db8::1/128"),
		netip.MustParsePrefix("2001:db8:1::/48"),
		netip.MustParsePrefix("192.0.2.9/32"), // unmapped
	}, list.Prefixes)
	assert.Equal(t, []string{"example.com", "ads.example.net", "tracker.example.org", "cdn.example.com",
		"hosts-a.example.com", "hosts-b.example.com", "hosts-c.example.com", "hosts-d.example.com"}, list.Domains)
	assert.Equal(t, 14, list.Count())
}

func TestParseListErrors(t *testing.T) {
	_, err := ParseList("192.0.2.1\nbad_entry!\n10.0.0.0/33\n0.0.0.0/0\n300.1.1.1\ntwo words\n")
	require.Error(t, err)
	assert.True(t, errors.Is(err, domain.ErrInvalidData))
	msg := err.Error()
	assert.Contains(t, msg, "5 invalid line(s)")
	for _, want := range []string{"line 2:", "line 3:", "line 4: default route", "line 5:", "line 6:"} {
		assert.Contains(t, msg, want)
	}
	assert.NotContains(t, msg, "line 1:")

	var many strings.Builder
	for i := range 15 {
		fmt.Fprintf(&many, "bad entry %d!\n", i)
	}
	_, err = ParseList(many.String())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "15 invalid line(s)")
	assert.Contains(t, err.Error(), "line 10:")
	assert.NotContains(t, err.Error(), "line 11:") // only the first 10 are reported
}
