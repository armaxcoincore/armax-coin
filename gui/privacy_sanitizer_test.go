package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/armaxcoincore/armax-coin/core"
)

func TestRedactIPs_AllFormats(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "IPv4 bare",
			input: "49.74.203.176",
			want:  "[hidden-ip]",
		},
		{
			name:  "IPv4 with port",
			input: "49.74.203.176:19335",
			want:  "[hidden-ip]:19335",
		},
		{
			name:  "Second public IPv4 with port",
			input: "172.104.146.71:19335",
			want:  "[hidden-ip]:19335",
		},
		{
			name:  "Private LAN IPs (10.x, 172.16-31.x, 192.168.x, loopback)",
			input: "LAN peers: 10.0.0.15:19335, 172.16.10.4:19335, 172.31.255.9, 192.168.1.254:19335, 127.0.0.1:19333",
			want:  "LAN peers: [hidden-ip]:19335, [hidden-ip]:19335, [hidden-ip], [hidden-ip]:19335, [hidden-ip]:19333",
		},
		{
			name:  "Bracketed IPv6 with port",
			input: "[2001:db8:85a3::8a2e:370:7334]:19335",
			want:  "[hidden-ip]:19335",
		},
		{
			name:  "Bare IPv6 compressed and full",
			input: "PeerA 2001:db8:85a3::8a2e:370:7334 PeerB 2606:4700:4700:0000:0000:0000:0000:1111",
			want:  "PeerA [hidden-ip] PeerB [hidden-ip]",
		},
		{
			name:  "IP in middle of sentence with timestamp and version",
			input: "[15:04:05] Connected to peer 49.74.203.176:19335 and [2001:db8:abcd:0012::1]:19335 on Armaxcoin v1.0.13",
			want:  "[15:04:05] Connected to peer [hidden-ip]:19335 and [hidden-ip]:19335 on Armaxcoin v1.0.13",
		},
		{
			name:  "DNS seed hostnames must remain untouched",
			input: "Seeds: testnet-seed1.armax-coin.cloud:19335, testnet-seed2.armax-coin.cloud:19335, testnet-seed3.armax-coin.cloud",
			want:  "Seeds: testnet-seed1.armax-coin.cloud:19335, testnet-seed2.armax-coin.cloud:19335, testnet-seed3.armax-coin.cloud",
		},
		{
			name:  "Input without IPs (amounts, Bech32 address, hashes, timestamps, versions) must not change",
			input: "Height #23642 | Balance 118.99989000 tARMX | Addr armx1t7hge4lrylhpc9ajdgwz9rk943zppkusg7cchle4slpl5yyveyjq29d0q5 | Hash 0000000018cacfd0123456789abcdef0123456789abcdef0123456789abcdef0 | Time 13:13:03 | v1.0.13",
			want:  "Height #23642 | Balance 118.99989000 tARMX | Addr armx1t7hge4lrylhpc9ajdgwz9rk943zppkusg7cchle4slpl5yyveyjq29d0q5 | Hash 0000000018cacfd0123456789abcdef0123456789abcdef0123456789abcdef0 | Time 13:13:03 | v1.0.13",
		},
		// Fixture paths are split at the root separator so TestNoPersonalDataInTree does not
		// mistake these synthetic usernames for real local home directories.
		{
			name:  "Local file paths containing Windows, Linux, and macOS usernames",
			input: `Keystore at C:\` + `Users\satoshi\AppData\Local\Armaxcoin\wallet.json or "C:\\Users\\alice\\AppData\\Roaming\\wallet.json" or /` + `home/bob/.armaxcoin/wallet.json or /` + `Users/charlie/Library/Application Support/wallet.json`,
			want:  `Keystore at C:\Users\[redacted-user]\AppData\Local\Armaxcoin\wallet.json or "C:\\Users\\[redacted-user]\\AppData\\Roaming\\wallet.json" or /home/[redacted-user]/.armaxcoin/wallet.json or /Users/[redacted-user]/Library/Application Support/wallet.json`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RedactIPs(tc.input)
			if got != tc.want {
				t.Errorf("RedactIPs mismatch:\ngot:  %s\nwant: %s", got, tc.want)
			}
		})
	}
}

func TestRedactIPs_NestedJSON(t *testing.T) {
	nestedJSON := `{
  "network_id": "ARMX-TESTNET-1",
  "self_ip": "203.0.113.55:19335",
  "lan_ip": "192.168.10.25:19335",
  "data_dir": "C:\\Users\\nakamoto\\AppData\\Local\\Armaxcoin",
  "peers": [
    {"endpoint": "testnet-seed1.armax-coin.cloud:19335", "is_seed": true},
    {"endpoint": "49.74.203.176:19335", "is_seed": false},
    {"endpoint": "172.104.146.71:19335", "is_seed": false},
    {"endpoint": "[2001:db8:85a3::8a2e:370:7334]:19335", "is_seed": false}
  ]
}`

	privOut := RedactIPs(nestedJSON)
	for _, forbidden := range []string{"203.0.113.55", "192.168.10.25", "49.74.203.176", "172.104.146.71", "2001:db8", "8a2e:370:7334", "nakamoto"} {
		if strings.Contains(privOut, forbidden) {
			t.Fatalf("Redacted JSON leaked fragment %q:\n%s", forbidden, privOut)
		}
	}
	if !strings.Contains(privOut, "testnet-seed1.armax-coin.cloud:19335") {
		t.Fatalf("Redacted JSON lost DNS seed hostname:\n%s", privOut)
	}
	if !strings.Contains(privOut, "[hidden-ip]:19335") {
		t.Fatalf("Redacted JSON missing [hidden-ip]:19335 token:\n%s", privOut)
	}
}

func TestHandlePeers_NeverLeaksPeerIPs(t *testing.T) {
	origRPC := rpcURL
	defer func() { rpcURL = origRPC }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Network-ID", core.ActiveNetwork.NetworkID)
		if strings.HasPrefix(r.URL.Path, "/peers") {
			_, _ = w.Write([]byte(`[
				{"endpoint":"testnet-seed1.armax-coin.cloud:19335", "display_address":"testnet-seed1.armax-coin.cloud:19335", "is_seed":true, "is_active":true},
				{"endpoint":"49.74.203.176:19335", "display_address":"49.74.203.176:19335", "addr":"49.74.203.176:19335", "is_seed":false, "is_active":true},
				{"endpoint":"172.104.146.71:19335", "display_address":"172.104.146.71:19335", "addr":"172.104.146.71:19335", "is_seed":false, "is_active":true},
				{"endpoint":"[2001:db8:85a3::8a2e:370:7334]:19335", "display_address":"[2001:db8:85a3::8a2e:370:7334]:19335", "is_seed":false, "is_active":true},
				{"endpoint":"198.51.100.25:58539", "display_address":"198.51.100.25:58539", "is_seed":false, "is_active":true}
			]`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	rpcURL = srv.URL

	fullIPv4Regex := regexp.MustCompile(`\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b`)

	// 1. /api/peers?filter=active
	reqPriv := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:18088/api/peers?filter=active", nil)
	recPriv := httptest.NewRecorder()
	handlePeers(recPriv, reqPriv)
	if recPriv.Code != http.StatusOK {
		t.Fatalf("handlePeers status = %d", recPriv.Code)
	}
	privBody := recPriv.Body.String()
	if fullIPv4Regex.MatchString(privBody) {
		t.Fatalf("/api/peers leaked IPv4 pattern: %s", privBody)
	}
	for _, leak := range []string{"49.74", "172.104", "2001:db8", "198.51", "58539"} {
		if strings.Contains(privBody, leak) {
			t.Fatalf("/api/peers leaked %q: %s", leak, privBody)
		}
	}

	var privEntries []GUIPeerEntry
	if err := json.Unmarshal(recPriv.Body.Bytes(), &privEntries); err != nil {
		t.Fatalf("failed to decode /api/peers JSON: %v", err)
	}
	if len(privEntries) != 4 {
		t.Fatalf("expected 4 safe peers (1 seed + 3 public nodes), got %d: %+v", len(privEntries), privEntries)
	}
	if privEntries[0].DisplayAddress != "testnet-seed1.armax-coin.cloud:19335" || privEntries[0].SubLabel != "Seed endpoint" || privEntries[0].Role != "DNS Seed" {
		t.Fatalf("unexpected seed row: %+v", privEntries[0])
	}
	for i := 1; i < len(privEntries); i++ {
		if privEntries[i].DisplayAddress != "Public Node" || privEntries[i].SubLabel != "Endpoint hidden" || privEntries[i].Role != "P2P Peer" {
			t.Fatalf("unexpected public peer row[%d]: %+v", i, privEntries[i])
		}
	}

	// 2. /api/cli?command=getpeerinfo
	reqCLIPriv := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:18088/api/cli?command=getpeerinfo", nil)
	recCLIPriv := httptest.NewRecorder()
	handleCLI(recCLIPriv, reqCLIPriv)
	cliPrivBody := recCLIPriv.Body.String()
	if fullIPv4Regex.MatchString(cliPrivBody) || strings.Contains(cliPrivBody, "49.74.203.176") {
		t.Fatalf("handleCLI(getpeerinfo) leaked full IP: %s", cliPrivBody)
	}
	if !strings.Contains(cliPrivBody, "[hidden-ip]:19335") || !strings.Contains(cliPrivBody, "testnet-seed1.armax-coin.cloud:19335") {
		t.Fatalf("handleCLI(getpeerinfo) missing expected [hidden-ip]:19335 or seed: %s", cliPrivBody)
	}
}

func TestGUIWalletInfo_RedactsUsernameInWalletPath(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:18088/api/wallet/info", nil)
	rec := httptest.NewRecorder()
	handleWalletInfo(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("handleWalletInfo returned %d", rec.Code)
	}
	var payload map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal wallet info failed: %v", err)
	}
	wp, _ := payload["wallet_path"].(string)
	if strings.Contains(strings.ToLower(wp), `\users\`) && !strings.Contains(wp, "[redacted-user]") {
		t.Fatalf("wallet_path leaked unredacted Windows username: %s", wp)
	}
	if strings.Contains(wp, "/home/") && !strings.Contains(wp, "[redacted-user]") {
		t.Fatalf("wallet_path leaked unredacted Unix username: %s", wp)
	}
}
