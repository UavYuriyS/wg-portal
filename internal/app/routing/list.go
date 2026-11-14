package routing

import (
	"bufio"
	"fmt"
	"net/netip"
	"strings"

	"github.com/h44z/wg-portal/internal/domain"
)

const maxListErrors = 10

// ParsedList holds the entries of one routing list.
type ParsedList struct {
	Prefixes []netip.Prefix // masked, IPv4 unmapped
	Domains  []string       // lower case, no leading "*." or "." and no trailing dot
}

func (l ParsedList) Count() int { return len(l.Prefixes) + len(l.Domains) }

// hosts file sink addresses: "0.0.0.0 ads.example.com" blocks ads.example.com
var hostsSinks = map[string]bool{"0.0.0.0": true, "127.0.0.1": true, "::": true, "::1": true}

// ParseList parses one entry per line: an IP, a CIDR or a domain (matches itself and its subdomains, a
// leading "*." or "." is accepted). "#" starts a comment. Hosts file lines take the domain names, so public
// blocklists upload as-is; hosts lines of other addresses ("255.255.255.255 broadcasthost") and dotless
// names ("localhost") are ignored. Any invalid line rejects the whole list with the first 10 errors.
func ParseList(content string) (ParsedList, error) {
	var list ParsedList
	var errs []string
	scanner := bufio.NewScanner(strings.NewReader(content))
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for lineNo := 1; scanner.Scan(); lineNo++ {
		line, _, _ := strings.Cut(scanner.Text(), "#")
		fields := strings.Fields(line)
		var err error
		switch {
		case len(fields) == 0:
			continue
		case len(fields) == 1:
			err = list.addEntry(fields[0])
		default:
			err = list.addHostsLine(fields)
		}
		if err != nil {
			errs = append(errs, fmt.Sprintf("line %d: %v", lineNo, err))
		}
	}
	if err := scanner.Err(); err != nil {
		errs = append(errs, err.Error())
	}
	if len(errs) > 0 {
		total := len(errs)
		if total > maxListErrors {
			errs = errs[:maxListErrors]
		}
		return ParsedList{}, fmt.Errorf("%w: list rejected, %d invalid line(s):\n%s", domain.ErrInvalidData, total,
			strings.Join(errs, "\n"))
	}
	return list, nil
}

func (l *ParsedList) addEntry(entry string) error {
	if strings.Contains(entry, "/") {
		prefix, err := netip.ParsePrefix(entry)
		if err != nil {
			return fmt.Errorf("invalid CIDR %q", entry)
		}
		return l.addPrefix(netip.PrefixFrom(prefix.Addr().Unmap(), prefix.Bits()))
	}
	if addr, err := netip.ParseAddr(entry); err == nil {
		addr = addr.Unmap()
		return l.addPrefix(netip.PrefixFrom(addr, addr.BitLen()))
	}
	name, ok := normalizeDomain(entry)
	if !ok {
		return fmt.Errorf("invalid IP, CIDR or domain %q", entry)
	}
	l.Domains = append(l.Domains, name)
	return nil
}

func (l *ParsedList) addPrefix(prefix netip.Prefix) error {
	if !prefix.IsValid() || prefix.Bits() == 0 {
		return fmt.Errorf("default route %s is not allowed", prefix)
	}
	l.Prefixes = append(l.Prefixes, prefix.Masked())
	return nil
}

func (l *ParsedList) addHostsLine(fields []string) error {
	addr, err := netip.ParseAddr(fields[0])
	if err != nil {
		if strings.Contains(fields[0], "%") { // e.g. "fe80::1%lo0 localhost"
			return nil
		}
		return fmt.Errorf("expected one entry or a hosts file line, got %q", strings.Join(fields, " "))
	}
	if !hostsSinks[addr.String()] {
		return nil
	}
	for _, f := range fields[1:] {
		if !strings.Contains(strings.Trim(f, "."), ".") {
			continue // localhost, local, ip6-localhost ...
		}
		if _, err := netip.ParseAddr(f); err == nil {
			continue // "0.0.0.0 0.0.0.0"
		}
		name, ok := normalizeDomain(f)
		if !ok {
			return fmt.Errorf("invalid domain %q", f)
		}
		l.Domains = append(l.Domains, name)
	}
	return nil
}

// normalizeDomain lower-cases name and strips a leading "*." or "." and the trailing dot.
func normalizeDomain(name string) (string, bool) {
	name = strings.ToLower(name)
	name = strings.TrimPrefix(name, "*.")
	name = strings.TrimPrefix(name, ".")
	name = strings.TrimSuffix(name, ".")
	if name == "" || len(name) > 253 {
		return "", false
	}
	labels := strings.Split(name, ".")
	if strings.Trim(labels[len(labels)-1], "0123456789") == "" {
		return "", false // numeric top level label: a malformed IP like 300.1.1.1
	}
	for _, label := range labels {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
				return "", false
			}
		}
	}
	return name, true
}
