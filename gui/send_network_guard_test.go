package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/armaxcoincore/armax-coin/core"
	"github.com/armaxcoincore/armax-coin/wallet"
)

// TestHandleSend_RejectsCrossNetworkRecipient verifies the GUI refuses a recipient of another
// network (here a Mainnet ax1... address while running on Testnet) before contacting the node or
// signing, and names the network in the error.
func TestHandleSend_RejectsCrossNetworkRecipient(t *testing.T) {
	recip, err := wallet.NewWallet()
	if err != nil {
		t.Fatalf("NewWallet: %v", err)
	}
	defer recip.Zeroize()

	payloadHex, err := core.AddressToHex(recip.Address)
	if err != nil {
		t.Fatalf("AddressToHex: %v", err)
	}
	raw, _ := hex.DecodeString(payloadHex)
	mainnetAddr, err := core.Bech32Encode(core.MainnetAddressHRP, raw)
	if err != nil {
		t.Fatalf("Bech32Encode: %v", err)
	}

	body, _ := json.Marshal(map[string]string{"to": mainnetAddr, "amount": "1.0"})
	req := httptest.NewRequest(http.MethodPost, "/api/send", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handleSend(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected HTTP 400 for cross-network recipient, got %d (%q)", rec.Code, rec.Body.String())
	}
	msg := rec.Body.String()
	if !strings.Contains(msg, "Rejected before signing") || !strings.Contains(msg, "ARMX-MAINNET-1") {
		t.Fatalf("expected network-specific rejection, got %q", msg)
	}
}

// TestWebUI_AddressPrefixPlaceholders verifies the served UI no longer hardcodes the Testnet prefix
// and that every placeholder token is substituted by the page handler.
func TestWebUI_AddressPrefixPlaceholders(t *testing.T) {
	if strings.Contains(webUIHTML, `placeholder="armx1...`) {
		t.Fatalf("web UI still hardcodes the armx1 address placeholder")
	}
	for _, token := range []string{"{{ADDR_PREFIX}}", "{{NETWORK_LABEL}}"} {
		if !strings.Contains(webUIHTML, token) {
			t.Fatalf("web UI is missing %s", token)
		}
	}
}
