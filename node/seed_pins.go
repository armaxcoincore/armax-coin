package node

import (
	"fmt"
	"net"
	"strings"
)

// SetSeedFingerprints installs operator-supplied certificate fingerprints for seed nodes so
// the very first connection to a seed is authenticated instead of blindly trusted (TOFU).
// Keys are "host:port" (IP or DNS name); a DNS name is resolved now and tracked across later
// ReseedPeers rotations per SeedHost, rejecting mismatched certificates and failing closed on
// ambiguous multi-pin shared bare IPs. Peers without an entry keep the normal TOFU behavior.
func (p *PeerServer) SetSeedFingerprints(pins map[string]string) error {
	hostPins := make(map[string]string, len(pins))
	rawHosts := make(map[string]string, len(pins))
	for addr, fp := range pins {
		fp = strings.ToLower(strings.TrimSpace(fp))
		if len(fp) != 64 || strings.Trim(fp, "0123456789abcdef") != "" {
			return fmt.Errorf("seed fingerprint for %q must be 64 hex characters (SHA-256 of the SPKI)", addr)
		}
		trimmed := strings.TrimSpace(addr)
		if _, _, err := net.SplitHostPort(trimmed); err != nil {
			return fmt.Errorf("seed fingerprint key %q must be host:port", addr)
		}
		normKey := normalizePeerEndpoint(trimmed)
		hostPins[normKey] = fp
		rawHosts[normKey] = trimmed
	}

	// Acquire seedMu before seedPinMu (never concurrently nested here) to satisfy lock hierarchy.
	p.seedMu.RLock()
	resolveSeed := p.resolveSeed
	cachedHostToIPs := make(map[string][]string, len(p.seedHostToIPs))
	for k, ips := range p.seedHostToIPs {
		cachedHostToIPs[k] = append([]string(nil), ips...)
	}
	p.seedMu.RUnlock()
	if resolveSeed == nil {
		resolveSeed = resolveDNSSeed
	}

	hostResolved := make(map[string][]string, len(hostPins))
	for normKey := range hostPins {
		rawAddr := rawHosts[normKey]
		seenIP := make(map[string]struct{})
		var resolved []string
		appendIfValid := func(ep string) {
			normEP := normalizePeerEndpoint(ep)
			if normEP == "" {
				return
			}
			if _, exists := seenIP[normEP]; exists {
				return
			}
			seenIP[normEP] = struct{}{}
			resolved = append(resolved, normEP)
		}
		for _, ra := range resolveSeed(rawAddr, false) {
			appendIfValid(ra)
		}
		for _, ca := range cachedHostToIPs[rawAddr] {
			appendIfValid(ca)
		}
		for _, ca := range cachedHostToIPs[normKey] {
			appendIfValid(ca)
		}
		hostResolved[normKey] = resolved
	}

	p.seedPinMu.Lock()
	p.seedHostPins = hostPins
	p.seedHostResolvedIPs = hostResolved
	p.rebuildSeedPinIndexesLocked()
	p.seedPinMu.Unlock()
	return nil
}

// rebuildSeedPinIndexesLocked recomputes seedIPPins and backwards-compatible seedPins
// from seedHostPins and seedHostResolvedIPs while caller holds seedPinMu.Lock.
func (p *PeerServer) rebuildSeedPinIndexesLocked() {
	ipPins := make(map[string]map[string]struct{})
	addIPPin := func(ep, fp string) {
		normEP := normalizePeerEndpoint(ep)
		if normEP == "" {
			return
		}
		if ipPins[normEP] == nil {
			ipPins[normEP] = make(map[string]struct{})
		}
		ipPins[normEP][fp] = struct{}{}
	}

	for hostKey, fp := range p.seedHostPins {
		hostOnly, _, _ := net.SplitHostPort(hostKey)
		if net.ParseIP(hostOnly) != nil {
			addIPPin(hostKey, fp)
		}
		for _, ep := range p.seedHostResolvedIPs[hostKey] {
			addIPPin(ep, fp)
		}
	}

	legacyPins := make(map[string]string, len(p.seedHostPins)+len(ipPins))
	for hostKey, fp := range p.seedHostPins {
		legacyPins[hostKey] = fp
	}
	for ip, fps := range ipPins {
		if len(fps) == 1 {
			for onlyFP := range fps {
				legacyPins[ip] = onlyFP
			}
		} else {
			delete(legacyPins, ip)
		}
	}

	p.seedIPPins = ipPins
	p.seedPins = legacyPins
}

// syncSeedPinsFromHostMap propagates DNS seed IP rotations from reseedPeers into the pin index
// without holding seedMu, preserving host-scoped pin enforcement on newly rotated IPs.
func (p *PeerServer) syncSeedPinsFromHostMap(hostToIPs map[string][]string) {
	if len(hostToIPs) == 0 {
		return
	}
	p.seedPinMu.Lock()
	defer p.seedPinMu.Unlock()
	if len(p.seedHostPins) == 0 {
		return
	}
	updated := false
	for hostKey := range p.seedHostPins {
		ips, ok := hostToIPs[hostKey]
		if !ok || len(ips) == 0 {
			continue
		}
		if p.seedHostResolvedIPs == nil {
			p.seedHostResolvedIPs = make(map[string][]string)
		}
		copied := make([]string, 0, len(ips))
		seen := make(map[string]struct{}, len(ips))
		for _, ip := range ips {
			normIP := normalizePeerEndpoint(ip)
			if normIP == "" {
				continue
			}
			if _, exists := seen[normIP]; !exists {
				seen[normIP] = struct{}{}
				copied = append(copied, normIP)
			}
		}
		p.seedHostResolvedIPs[hostKey] = copied
		updated = true
	}
	if updated {
		p.rebuildSeedPinIndexesLocked()
	}
}

func (p *PeerServer) lookupPeerSeedHost(peerAddr string) string {
	normAddr := normalizePeerEndpoint(peerAddr)
	p.Lock.RLock()
	defer p.Lock.RUnlock()
	for _, table := range []map[string]*PeerInfo{p.triedPeers, p.newPeers} {
		if table == nil {
			continue
		}
		if info := table[peerAddr]; info != nil && info.SeedHost != "" {
			return info.SeedHost
		}
		if normAddr != peerAddr {
			if info := table[normAddr]; info != nil && info.SeedHost != "" {
				return info.SeedHost
			}
		}
	}
	return ""
}

// checkSeedPin rejects a connection whose certificate differs from the operator-pinned one.
func (p *PeerServer) checkSeedPin(peerAddr, fp string) error {
	return p.checkSeedPinWithHost(peerAddr, p.lookupPeerSeedHost(peerAddr), fp)
}

// checkSeedPinWithHost evaluates seed pin constraints strictly per originating SeedHost when known,
// falling back to resolved-IP lookup and failing closed on conflicting multi-host shared IPs.
func (p *PeerServer) checkSeedPinWithHost(peerAddr, seedHost, fp string) error {
	normAddr := normalizePeerEndpoint(peerAddr)
	normHost := normalizePeerEndpoint(seedHost)

	p.seedPinMu.RLock()
	defer p.seedPinMu.RUnlock()

	if want, ok := p.seedHostPins[normAddr]; ok {
		if strings.EqualFold(want, fp) {
			return nil
		}
		return fmt.Errorf("P2P TOFU verification failed for peer %s: certificate does not match the pinned seed fingerprint (pinned %s, got %s)", peerAddr, want, fp)
	}

	if normHost != "" {
		if want, ok := p.seedHostPins[normHost]; ok {
			if strings.EqualFold(want, fp) {
				return nil
			}
			return fmt.Errorf("P2P TOFU verification failed for peer %s (seed host %s): certificate does not match the pinned seed fingerprint (pinned %s, got %s)", peerAddr, normHost, want, fp)
		}
	}

	if fps, ok := p.seedIPPins[normAddr]; ok && len(fps) > 0 {
		if len(fps) > 1 {
			return fmt.Errorf("P2P TOFU fail-closed for shared IP peer %s: multiple distinct configured seed pins map to this IP without an unambiguous SeedHost", peerAddr)
		}
		for want := range fps {
			if strings.EqualFold(want, fp) {
				return nil
			}
			return fmt.Errorf("P2P TOFU verification failed for peer %s: certificate does not match the pinned seed fingerprint (pinned %s, got %s)", peerAddr, want, fp)
		}
	}

	// Legacy direct-map compatibility for internal tests writing seedPins without SetSeedFingerprints.
	if want, ok := p.seedPins[normAddr]; ok && want != "" {
		if strings.EqualFold(want, fp) {
			return nil
		}
		return fmt.Errorf("P2P TOFU verification failed for peer %s: certificate does not match the pinned seed fingerprint (pinned %s, got %s)", peerAddr, want, fp)
	}

	return nil
}
