// Lab-only fakes: an ipify-like "what is my IP" service on :8080 and a minimal reg.ru API v2 on :8081
// that keeps A records and writes them as a hosts file served by CoreDNS.
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
)

type regRuInput struct {
	Domains []struct {
		Dname string `json:"dname"`
	} `json:"domains"`
	Subdomain string `json:"subdomain"`
	Ipaddr    string `json:"ipaddr"`
	Content   string `json:"content"`
}

func main() {
	go func() {
		log.Fatal(http.ListenAndServe(":8080", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			host, _, _ := net.SplitHostPort(r.RemoteAddr)
			fmt.Fprint(w, host)
		})))
	}()

	hostsFile := os.Getenv("HOSTS_FILE")
	var mu sync.Mutex
	records := map[string]map[string]bool{}             // fqdn -> set of ips
	if raw, err := os.ReadFile(hostsFile); err == nil { // keep the records across a restart (lab redeploy)
		for _, line := range strings.Split(string(raw), "\n") {
			if f := strings.Fields(line); len(f) == 2 {
				if records[f[1]] == nil {
					records[f[1]] = map[string]bool{}
				}
				records[f[1]][f[0]] = true
			}
		}
	}
	write := func() {
		var lines []string
		for fqdn, ips := range records {
			for ip := range ips {
				lines = append(lines, ip+" "+fqdn)
			}
		}
		sort.Strings(lines)
		_ = os.WriteFile(hostsFile, []byte(strings.Join(lines, "\n")+"\n"), 0o644)
		log.Printf("hosts: %v", lines)
	}
	reply := func(w http.ResponseWriter, dname, result, code string) {
		_ = json.NewEncoder(w).Encode(map[string]any{"result": "success", "answer": map[string]any{
			"domains": []map[string]string{{"dname": dname, "result": result, "error_code": code}}}})
	}

	handler := func(add bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			_ = r.ParseForm()
			var in regRuInput
			if r.Form.Get("input_format") != "json" || json.Unmarshal([]byte(r.Form.Get("input_data")), &in) != nil ||
				len(in.Domains) != 1 {
				_ = json.NewEncoder(w).Encode(map[string]string{"result": "error", "error_code": "PARAMETER_INCORRECT"})
				return
			}
			dname := in.Domains[0].Dname
			fqdn := in.Subdomain + "." + dname
			mu.Lock()
			defer mu.Unlock()
			if records[fqdn] == nil {
				records[fqdn] = map[string]bool{}
			}
			switch {
			case add:
				records[fqdn][in.Ipaddr] = true
			case records[fqdn][in.Content]:
				delete(records[fqdn], in.Content)
			default:
				reply(w, dname, "error", "NO_SUCH_RR")
				return
			}
			write()
			reply(w, dname, "success", "")
		}
	}
	http.HandleFunc("/api/regru2/zone/get_resource_records", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		var in regRuInput
		if json.Unmarshal([]byte(r.Form.Get("input_data")), &in) != nil || len(in.Domains) != 1 {
			_ = json.NewEncoder(w).Encode(map[string]string{"result": "error", "error_code": "PARAMETER_INCORRECT"})
			return
		}
		dname := in.Domains[0].Dname
		mu.Lock()
		defer mu.Unlock()
		rrs := []map[string]string{}
		for fqdn, ips := range records {
			if sub, ok := strings.CutSuffix(fqdn, "."+dname); ok {
				for ip := range ips {
					rrs = append(rrs, map[string]string{"subname": sub, "rectype": "A", "content": ip})
				}
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"result": "success", "answer": map[string]any{
			"domains": []map[string]any{{"dname": dname, "result": "success", "rrs": rrs}}}})
	})
	http.HandleFunc("/api/regru2/zone/add_alias", handler(true))
	http.HandleFunc("/api/regru2/zone/remove_record", handler(false))
	write()
	log.Fatal(http.ListenAndServe(":8081", nil))
}
