package main

import (
	"encoding/json"
	"net"
	"os"
	"os/user"
	"regexp"
	"strconv"
	"strings"

	"github.com/armaxcoincore/armax-coin/core"
)

// GUIPeerEntry represents a privacy-filtered peer row sent from the GUI backend to the frontend table.
type GUIPeerEntry struct {
	DisplayAddress string `json:"display_address"`
	Endpoint       string `json:"endpoint"`
	SubLabel       string `json:"sub_label"`
	Role           string `json:"role"`
	Status         string `json:"status"`
	IsSeed         bool   `json:"is_seed"`
	IsActive       bool   `json:"is_active"`
}

var (
	// Matches bracketed IPv6 tokens such as [2001:db8::1]:19335 or [::1]
	reBracketedIPv6 = regexp.MustCompile(`\[([0-9a-fA-F:.]+(?:%[0-9a-zA-Z_.-]+)?)\](:\d{1,5})?`)

	// Matches candidate bare IPv6 tokens (at least two colons)
	reBareIPv6Candidate = regexp.MustCompile(`[0-9a-fA-F:]*:[0-9a-fA-F:]+(?:\.\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3})?`)

	// Matches candidate IPv4 dotted-quad addresses
	reIPv4Candidate = regexp.MustCompile(`\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}`)

	// Matches Windows user profile paths: C:\Users\<username>, C:\\Users\\<username>, C:/Users/<username>
	reWinUsersPath = regexp.MustCompile(`(?i)([a-z]:(?:\\\\|\\|/)users(?:\\\\|\\|/))([^\\/:*?"<>|\r\n\t ]+)`)

	// Matches legacy Windows Documents and Settings paths
	reWinDocsPath = regexp.MustCompile(`(?i)([a-z]:(?:\\\\|\\|/)documents and settings(?:\\\\|\\|/))([^\\/:*?"<>|\r\n\t ]+)`)

	// Matches Unix/Linux /home/<username> and macOS /Users/<username> paths
	reUnixUserPath = regexp.MustCompile(`((?:^|[\s"'=:(])(?:/home/|/Users/))([^\\/:*?"<>|\r\n\t ]+)`)
)

// RedactIPs sanitizes all IPv4/IPv6 addresses (including ip:port, [ipv6]:port, LAN/private IPs)
// and local username file paths in free text or JSON into "[hidden-ip]".
// DNS seed hostnames and ports are preserved.
func RedactIPs(input string) string {
	if input == "" {
		return ""
	}

	out := RedactLocalPaths(input)

	// 1. Replace bracketed IPv6: [ipv6]:port or [ipv6]
	out = reBracketedIPv6.ReplaceAllStringFunc(out, func(m string) string {
		sub := reBracketedIPv6.FindStringSubmatch(m)
		if len(sub) < 2 {
			return m
		}
		rawIP := stripIPv6Zone(sub[1])
		portPart := ""
		if len(sub) >= 3 {
			portPart = sub[2]
		}
		if net.ParseIP(rawIP) == nil {
			return m
		}
		return "[hidden-ip]" + portPart
	})

	// 2. Replace bare IPv6 tokens (not inside brackets)
	out = replaceBareIPv6Tokens(out)

	// 3. Replace IPv4 addresses (including ip:port)
	out = replaceIPv4Tokens(out)

	return out
}

// RedactLocalPaths masks local usernames inside Windows, Linux, and macOS file paths.
func RedactLocalPaths(input string) string {
	if input == "" {
		return ""
	}
	out := input

	// Dynamic OS home directory and current username redaction
	if homeDir, err := os.UserHomeDir(); err == nil && strings.TrimSpace(homeDir) != "" {
		cleanHome := strings.TrimSpace(homeDir)
		base := ""
		if u, uErr := user.Current(); uErr == nil && u != nil && u.Username != "" {
			parts := strings.Split(u.Username, `\`)
			base = parts[len(parts)-1]
		}
		if base != "" && !strings.EqualFold(base, "[redacted-user]") {
			redactedHome := replaceLastPathSegment(cleanHome, base, "[redacted-user]")
			if redactedHome != cleanHome {
				out = strings.ReplaceAll(out, cleanHome, redactedHome)
				escapedHome := strings.ReplaceAll(cleanHome, `\`, `\\`)
				escapedRedacted := strings.ReplaceAll(redactedHome, `\`, `\\`)
				if escapedHome != cleanHome {
					out = strings.ReplaceAll(out, escapedHome, escapedRedacted)
				}
				slashHome := strings.ReplaceAll(cleanHome, `\`, `/`)
				slashRedacted := strings.ReplaceAll(redactedHome, `\`, `/`)
				if slashHome != cleanHome {
					out = strings.ReplaceAll(out, slashHome, slashRedacted)
				}
			}
		}
	}

	out = reWinUsersPath.ReplaceAllStringFunc(out, func(m string) string {
		sub := reWinUsersPath.FindStringSubmatch(m)
		if len(sub) < 3 || sub[2] == "[redacted-user]" {
			return m
		}
		return sub[1] + "[redacted-user]"
	})

	out = reWinDocsPath.ReplaceAllStringFunc(out, func(m string) string {
		sub := reWinDocsPath.FindStringSubmatch(m)
		if len(sub) < 3 || sub[2] == "[redacted-user]" {
			return m
		}
		return sub[1] + "[redacted-user]"
	})

	out = reUnixUserPath.ReplaceAllStringFunc(out, func(m string) string {
		sub := reUnixUserPath.FindStringSubmatch(m)
		if len(sub) < 3 || sub[2] == "[redacted-user]" {
			return m
		}
		return sub[1] + "[redacted-user]"
	})

	return out
}

func replaceLastPathSegment(pathStr, oldSeg, newSeg string) string {
	idx := strings.LastIndex(strings.ToLower(pathStr), strings.ToLower(oldSeg))
	if idx < 0 {
		return pathStr
	}
	return pathStr[:idx] + newSeg + pathStr[idx+len(oldSeg):]
}

func stripIPv6Zone(s string) string {
	if idx := strings.IndexByte(s, '%'); idx >= 0 {
		return s[:idx]
	}
	return s
}

func isValidBareIPv6String(s string) (net.IP, bool) {
	clean := stripIPv6Zone(s)
	if !strings.Contains(clean, ":") {
		return nil, false
	}
	hasDoubleColon := strings.Contains(clean, "::")
	colonCount := strings.Count(clean, ":")
	if !hasDoubleColon && colonCount < 6 {
		return nil, false
	}
	ip := net.ParseIP(clean)
	if ip == nil {
		return nil, false
	}
	return ip, true
}

func replaceBareIPv6Tokens(s string) string {
	matches := reBareIPv6Candidate.FindAllStringIndex(s, -1)
	if len(matches) == 0 {
		return s
	}

	var b strings.Builder
	b.Grow(len(s))
	lastIdx := 0

	for _, loc := range matches {
		start, end := loc[0], loc[1]
		// Skip if inside brackets (already handled) or adjacent to identifier characters
		if start > 0 {
			prev := s[start-1]
			if prev == '[' || isAlphaNumOrUnderscore(prev) {
				continue
			}
		}
		if end < len(s) {
			next := s[end]
			if next == ']' || isAlphaNumOrUnderscore(next) {
				continue
			}
		}

		cand := s[start:end]
		if _, ok := isValidBareIPv6String(cand); ok {
			b.WriteString(s[lastIdx:start])
			b.WriteString("[hidden-ip]")
			lastIdx = end
			continue
		}

		// Also check if cand is an unbracketed full 8-group IPv6 with trailing :port
		if lastColon := strings.LastIndexByte(cand, ':'); lastColon > 0 {
			hostPart := cand[:lastColon]
			portPart := cand[lastColon+1:]
			if isValidPortString(portPart) {
				if _, ok := isValidBareIPv6String(hostPart); ok {
					b.WriteString(s[lastIdx:start])
					b.WriteString("[hidden-ip]:" + portPart)
					lastIdx = end
				}
			}
		}
	}

	if lastIdx == 0 {
		return s
	}
	b.WriteString(s[lastIdx:])
	return b.String()
}

func replaceIPv4Tokens(s string) string {
	matches := reIPv4Candidate.FindAllStringIndex(s, -1)
	if len(matches) == 0 {
		return s
	}

	var b strings.Builder
	b.Grow(len(s))
	lastIdx := 0

	for _, loc := range matches {
		start, end := loc[0], loc[1]
		if start > 0 {
			prev := s[start-1]
			if prev == '.' || isAlphaNumOrUnderscore(prev) {
				continue
			}
		}
		if end < len(s) {
			next := s[end]
			if next == '.' || isAlphaNumOrUnderscore(next) {
				continue
			}
		}

		cand := s[start:end]
		if _, ok := parseStrictIPv4(cand); !ok {
			continue
		}

		b.WriteString(s[lastIdx:start])
		b.WriteString("[hidden-ip]")
		lastIdx = end
	}

	if lastIdx == 0 {
		return s
	}
	b.WriteString(s[lastIdx:])
	return b.String()
}

func parseStrictIPv4(s string) ([4]byte, bool) {
	var out [4]byte
	parts := strings.Split(s, ".")
	if len(parts) != 4 {
		return out, false
	}
	for i, p := range parts {
		if len(p) == 0 || len(p) > 3 {
			return out, false
		}
		if len(p) > 1 && p[0] == '0' {
			return out, false
		}
		val, err := strconv.Atoi(p)
		if err != nil || val < 0 || val > 255 {
			return out, false
		}
		out[i] = byte(val)
	}
	return out, true
}

func isValidPortString(s string) bool {
	if len(s) == 0 || len(s) > 5 {
		return false
	}
	p, err := strconv.Atoi(s)
	return err == nil && p >= 1 && p <= 65535
}

func isAlphaNumOrUnderscore(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_' || c == '-'
}

// isConfiguredOrDNSSeed checks whether an endpoint belongs to the configured seed list or is a DNS hostname seed.
func isConfiguredOrDNSSeed(endpoint string, defaultSeeds []string) bool {
	ep := strings.TrimSpace(endpoint)
	if ep == "" {
		return false
	}
	for _, s := range defaultSeeds {
		sc := strings.TrimSpace(s)
		if strings.EqualFold(sc, ep) {
			return true
		}
		if host, _, err := net.SplitHostPort(sc); err == nil && host != "" {
			if strings.EqualFold(host, ep) {
				return true
			}
			if epHost, _, epErr := net.SplitHostPort(ep); epErr == nil && strings.EqualFold(host, epHost) {
				return true
			}
		}
	}
	host := ep
	if h, _, err := net.SplitHostPort(ep); err == nil && h != "" {
		host = h
	}
	host = strings.Trim(host, "[]")
	if net.ParseIP(stripIPv6Zone(host)) == nil && strings.Contains(host, ".") {
		return true
	}
	return false
}

// buildGUIPeerEntries converts raw /peers JSON from armcoind into privacy-filtered GUIPeerEntry rows.
// Ephemeral client ports are excluded first via filterSafePeersJSON, and public peer IPs are hidden at the source.
func buildGUIPeerEntries(raw []byte, netProfile core.NetworkProfile) []GUIPeerEntry {
	safeRaw := filterSafePeersJSON(raw, netProfile)

	var strPeers []string
	if err := json.Unmarshal(safeRaw, &strPeers); err == nil {
		entries := make([]GUIPeerEntry, 0, len(strPeers))
		for _, p := range strPeers {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			if isConfiguredOrDNSSeed(p, netProfile.DefaultSeeds) {
				entries = append(entries, GUIPeerEntry{
					DisplayAddress: p,
					Endpoint:       p,
					SubLabel:       "Seed endpoint",
					Role:           "DNS Seed",
					Status:         "Active",
					IsSeed:         true,
					IsActive:       true,
				})
				continue
			}
			entries = append(entries, GUIPeerEntry{
				DisplayAddress: "Public Node",
				Endpoint:       "Public Node",
				SubLabel:       "Endpoint hidden",
				Role:           "P2P Peer",
				Status:         "Active",
				IsSeed:         false,
				IsActive:       true,
			})
		}
		return entries
	}

	var objPeers []map[string]interface{}
	if err := json.Unmarshal(safeRaw, &objPeers); err == nil {
		entries := make([]GUIPeerEntry, 0, len(objPeers))
		for _, obj := range objPeers {
			addr, _ := obj["display_address"].(string)
			if addr == "" {
				addr, _ = obj["endpoint"].(string)
			}
			if addr == "" {
				addr, _ = obj["addr"].(string)
			}
			if addr == "" {
				addr, _ = obj["address"].(string)
			}
			if addr == "" {
				addr, _ = obj["peer"].(string)
			}
			addr = strings.TrimSpace(addr)
			if addr == "" {
				continue
			}
			isSeedFlag, _ := obj["is_seed"].(bool)
			isSeed := isSeedFlag || isConfiguredOrDNSSeed(addr, netProfile.DefaultSeeds)

			isActive := true
			if act, ok := obj["is_active"].(bool); ok {
				isActive = act
			}
			status := "Active"
			if !isActive {
				status = "Known"
			}

			if isSeed {
				seedDisp := addr
				// If a seed object contains a bare IP instead of a DNS hostname, redact the IP.
				if host, _, splitErr := net.SplitHostPort(seedDisp); splitErr == nil {
					if net.ParseIP(stripIPv6Zone(strings.Trim(host, "[]"))) != nil {
						seedDisp = RedactIPs(seedDisp)
					}
				} else if net.ParseIP(stripIPv6Zone(strings.Trim(seedDisp, "[]"))) != nil {
					seedDisp = RedactIPs(seedDisp)
				}
				entries = append(entries, GUIPeerEntry{
					DisplayAddress: seedDisp,
					Endpoint:       seedDisp,
					SubLabel:       "Seed endpoint",
					Role:           "DNS Seed",
					Status:         status,
					IsSeed:         true,
					IsActive:       isActive,
				})
				continue
			}

			entries = append(entries, GUIPeerEntry{
				DisplayAddress: "Public Node",
				Endpoint:       "Public Node",
				SubLabel:       "Endpoint hidden",
				Role:           "P2P Peer",
				Status:         status,
				IsSeed:         false,
				IsActive:       isActive,
			})
		}
		return entries
	}

	return []GUIPeerEntry{}
}

// formatPeersTableJSON returns the JSON payload for /api/peers with all peer IPs hidden at the backend source.
func formatPeersTableJSON(raw []byte, netProfile core.NetworkProfile) []byte {
	entries := buildGUIPeerEntries(raw, netProfile)
	if entries == nil {
		entries = []GUIPeerEntry{}
	}
	out, err := json.Marshal(entries)
	if err != nil {
		return []byte("[]")
	}
	return out
}
