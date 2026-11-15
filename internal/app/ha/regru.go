package ha

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/h44z/wg-portal/internal/config"
)

// DnsUpdater points the VPN DNS name at the active node.
type DnsUpdater interface {
	// SetA makes ip the A record of the VPN name and removes the given old addresses.
	SetA(ctx context.Context, ip string, oldIps []string) error
}

// RegRuDns updates an A record through the reg.ru API v2 (https://www.reg.ru/reseller/api2doc).
// The caller IP must be allowed in the reg.ru account API settings.
// shortcut: reg.ru is the only provider, add an implementation of DnsUpdater for others when needed.
type RegRuDns struct {
	cfg    config.HaRegRuConfig
	client *http.Client
}

func NewRegRuDns(cfg config.HaRegRuConfig) *RegRuDns {
	return &RegRuDns{cfg: cfg, client: &http.Client{Timeout: 30 * time.Second}}
}

type regRuResponse struct {
	Result    string `json:"result"`
	ErrorCode string `json:"error_code"`
	ErrorText string `json:"error_text"`
	Answer    struct {
		Domains []struct {
			Dname     string `json:"dname"`
			Result    string `json:"result"`
			ErrorCode string `json:"error_code"`
			ErrorText string `json:"error_text"`
			Rrs       []struct {
				Subname string `json:"subname"`
				Rectype string `json:"rectype"`
				Content string `json:"content"`
			} `json:"rrs"`
		} `json:"domains"`
	} `json:"answer"`
}

func (d *RegRuDns) SetA(ctx context.Context, ip string, oldIps []string) error {
	// list first: adding an A record that already exists fails, e.g. on fail-back after a partial update.
	// The listing only ever skips the add: if it is misparsed the add fails loudly, removal never depends on it.
	r, err := d.call(ctx, "zone/get_resource_records", map[string]any{})
	if err != nil {
		return fmt.Errorf("list records of %s: %w", d.cfg.Zone, err)
	}
	present := map[string]bool{}
	for _, dom := range r.Answer.Domains {
		for _, rr := range dom.Rrs {
			if rr.Subname == d.cfg.Subdomain && rr.Rectype == "A" {
				present[rr.Content] = true
			}
		}
	}

	// add first, then remove: the name never resolves to nothing during the switch
	if !present[ip] {
		_, err := d.call(ctx, "zone/add_alias", map[string]any{"subdomain": d.cfg.Subdomain, "ipaddr": ip})
		if err != nil {
			return fmt.Errorf("add A %s.%s -> %s: %w", d.cfg.Subdomain, d.cfg.Zone, ip, err)
		}
	}
	var errs []string
	for _, old := range oldIps {
		if old == "" || old == ip {
			continue
		}
		_, err := d.call(ctx, "zone/remove_record",
			map[string]any{"subdomain": d.cfg.Subdomain, "record_type": "A", "content": old})
		if err != nil {
			errs = append(errs, err.Error()) // the record may simply not exist
		}
	}
	if len(errs) > 0 {
		return &PartialDnsError{Errors: errs}
	}
	return nil
}

// PartialDnsError means the new record was added but some old ones could not be removed.
type PartialDnsError struct{ Errors []string }

func (e *PartialDnsError) Error() string {
	return "removing old A records: " + strings.Join(e.Errors, "; ")
}

func (d *RegRuDns) call(ctx context.Context, method string, params map[string]any) (*regRuResponse, error) {
	params["username"] = d.cfg.Username
	params["password"] = d.cfg.Password
	params["domains"] = []map[string]string{{"dname": d.cfg.Zone}}
	params["output_content_type"] = "plain"
	input, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	form := url.Values{"input_format": {"json"}, "input_data": {string(input)}}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(d.cfg.ApiUrl, "/")+"/"+method,
		strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := d.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("reg.ru %s: http %s", method, resp.Status)
	}
	var r regRuResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("reg.ru %s: invalid response: %w", method, err)
	}
	if r.Result != "success" {
		return nil, fmt.Errorf("reg.ru %s: %s %s", method, r.ErrorCode, r.ErrorText)
	}
	for _, dom := range r.Answer.Domains {
		if dom.Result != "success" {
			return nil, fmt.Errorf("reg.ru %s for %s: %s %s", method, dom.Dname, dom.ErrorCode, dom.ErrorText)
		}
	}
	return &r, nil
}
