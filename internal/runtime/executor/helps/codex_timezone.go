package helps

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/proxyutil"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"golang.org/x/sync/singleflight"
)

// alignEnvironmentTime updates supported Codex time-context XML in-place.
// Dates remain client facts; only the root timezone field is replaced.
func alignEnvironmentTime(body []byte, zone string) []byte {
	if !gjson.ValidBytes(body) {
		return body
	}
	updated := body
	for i, item := range gjson.GetBytes(body, "input").Array() {
		role := item.Get("role").String()
		if role != "user" && role != "developer" {
			continue
		}
		content := item.Get("content")
		prefix := fmt.Sprintf("input.%d.content", i)
		contextTag := "environment_context"
		contentKind := "environments.environment_context"
		if role == "developer" {
			contextTag = "codex_apps_client_time_context"
			contentKind = "additional_content.codex_apps_client_time_context"
		}
		kindValue := item.Get("internal_chat_message_metadata_passthrough.content_item_kinds")
		kinds := kindValue.Array()
		for j, part := range content.Array() {
			if part.Get("type").String() != "input_text" {
				continue
			}
			if kindValue.IsArray() && (j >= len(kinds) || kinds[j].String() != contentKind) {
				continue
			}
			text := part.Get("text")
			if text.Type != gjson.String {
				continue
			}
			aligned := alignTimeContextText(text.String(), contextTag, zone)
			if aligned != text.String() {
				if next, err := sjson.SetBytes(updated, fmt.Sprintf("%s.%d.text", prefix, j), aligned); err == nil {
					updated = next
				}
			}
		}
	}
	return updated
}

func alignTimeContextText(text, contextTag, zone string) string {
	if zone == "" || zone == "Local" {
		return text
	}
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, "<"+contextTag+">") || !strings.HasSuffix(trimmed, "</"+contextTag+">") {
		return text
	}
	// Validate the whole document before applying byte-range replacements.
	decoder := xml.NewDecoder(strings.NewReader(text))
	type span struct{ start, end int }
	var spans []span
	depth, roots, start := 0, 0, -1
	for {
		before := int(decoder.InputOffset())
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return text
		}
		switch node := token.(type) {
		case xml.StartElement:
			if depth == 0 {
				roots++
				if roots != 1 || node.Name.Local != contextTag || node.Name.Space != "" {
					return text
				}
			}
			depth++
			if depth == 2 && node.Name.Local == "timezone" {
				start = before
			}
		case xml.EndElement:
			if depth == 2 && start >= 0 {
				spans = append(spans, span{start, int(decoder.InputOffset())})
				start = -1
			}
			depth--
		case xml.CharData:
			if depth == 0 && strings.TrimSpace(string(node)) != "" {
				return text
			}
		case xml.Directive:
			return text
		}
	}
	if depth != 0 || roots != 1 {
		return text
	}
	for i := len(spans) - 1; i >= 0; i-- {
		r := spans[i]
		text = text[:r.start] + "<timezone>" + zone + "</timezone>" + text[r.end:]
	}
	return text
}

// ApplyCodexRequestLocation is shared by HTTP, compact, and WebSocket preparation.
// Account off disables the feature; egress uses the selected route; on uses the
// account/proxy location before the global one; inherit follows the global switch.
func ApplyCodexRequestLocation(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, body []byte) []byte {
	if cfg == nil || !gjson.ValidBytes(body) {
		return body
	}
	policy := config.CodexRequestLocationPolicy{Mode: "inherit"}
	if auth != nil {
		var err error
		policy, err = config.CodexRequestLocationFromMetadata(auth.Metadata)
		if err != nil {
			return body
		}
	}
	var location codexRequestLocation
	switch policy.Mode {
	case "off":
		return body
	case "egress":
		location = codexTimezoneCache.resolve(ctx, cfg, auth)
	case "on":
		if policy.Location != nil {
			location = codexRequestLocation(*policy.Location)
		} else if cfg.Codex.RequestLocation.CodexRequestLocation.Validate() == nil {
			location = codexRequestLocation(cfg.Codex.RequestLocation.CodexRequestLocation)
		}
	case "inherit":
		if !cfg.Codex.RequestLocation.Enabled {
			return body
		}
		location = codexRequestLocation(cfg.Codex.RequestLocation.CodexRequestLocation)
	default:
		return body
	}
	if location.Timezone == "" {
		return body
	}
	body = alignEnvironmentTime(body, location.Timezone)
	return alignWebSearchLocation(body, location)
}

type timezoneEntry struct {
	zone  codexRequestLocation
	until time.Time
}
type timezoneResolver struct {
	mu      sync.Mutex
	entries map[[32]byte]timezoneEntry
	group   singleflight.Group
	now     func() time.Time
	lookup  func(context.Context, *config.Config, *cliproxyauth.Auth) codexRequestLocation
}

var codexTimezoneCache = &timezoneResolver{entries: make(map[[32]byte]timezoneEntry), now: time.Now, lookup: lookupEgressTimezone}

func (r *timezoneResolver) resolve(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth) codexRequestLocation {
	id := ""
	if auth != nil {
		id = auth.ID
	}
	// Include injected transport identity so different execution routes cannot share a result.
	key := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%p", id, effectiveProxyURL(ctx, cfg, auth), ctx.Value("cliproxy.roundtripper"))))
	get := func() (codexRequestLocation, bool) {
		r.mu.Lock()
		defer r.mu.Unlock()
		e, ok := r.entries[key]
		return e.zone, ok && r.now().Before(e.until)
	}
	if zone, ok := get(); ok {
		return zone
	}
	ch := r.group.DoChan(fmt.Sprintf("%x", key), func() (any, error) {
		if zone, ok := get(); ok {
			return zone, nil
		}
		zone := r.lookup(ctx, cfg, auth)
		ttl := 6 * time.Hour
		if zone.Timezone == "" {
			ttl = time.Minute
		}
		r.mu.Lock()
		if len(r.entries) >= 1024 {
			for k, e := range r.entries {
				if !r.now().Before(e.until) {
					delete(r.entries, k)
				}
			}
			if len(r.entries) >= 1024 {
				for k := range r.entries {
					delete(r.entries, k)
					break
				}
			}
		}
		r.entries[key] = timezoneEntry{zone: zone, until: r.now().Add(ttl)}
		r.mu.Unlock()
		return zone, nil
	})
	select {
	case <-ctx.Done():
		return codexRequestLocation{}
	case result := <-ch:
		zone, _ := result.Val.(codexRequestLocation)
		return zone
	}
}

func lookupEgressTimezone(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth) codexRequestLocation {
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	proxy := effectiveProxyURL(ctx, cfg, auth)
	if proxy != "" {
		transport, _, err := proxyutil.BuildHTTPTransport(proxy)
		if err != nil || transport == nil {
			return codexRequestLocation{}
		}
		defer transport.CloseIdleConnections()
		client.Transport = transport
	} else if rt, ok := ctx.Value("cliproxy.roundtripper").(http.RoundTripper); ok {
		client.Transport = rt
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://ipwho.is/?fields=success,country_code,region,city,timezone.id", nil)
	if err != nil {
		return codexRequestLocation{}
	}
	resp, err := client.Do(req)
	if err != nil {
		return codexRequestLocation{}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return codexRequestLocation{}
	}
	var result struct {
		Success  bool   `json:"success"`
		Country  string `json:"country_code"`
		Region   string `json:"region"`
		City     string `json:"city"`
		Timezone struct {
			ID string `json:"id"`
		} `json:"timezone"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 16384)).Decode(&result) != nil || !result.Success {
		return codexRequestLocation{}
	}
	zone := strings.TrimSpace(result.Timezone.ID)
	if zone == "" || zone == "Local" {
		return codexRequestLocation{}
	}
	if _, err = time.LoadLocation(zone); err != nil {
		return codexRequestLocation{}
	}
	return codexRequestLocation{Timezone: zone, Country: strings.TrimSpace(result.Country), Region: strings.TrimSpace(result.Region), City: strings.TrimSpace(result.City)}
}

// codexRequestLocation is the route-scoped GeoIP projection shared by both rewrites.
type codexRequestLocation struct {
	Country  string
	Region   string
	City     string
	Timezone string
}

func alignWebSearchLocation(body []byte, location codexRequestLocation) []byte {
	// A timezone alone cannot identify a city. Preserve the original search location
	// when GeoIP has not supplied a complete location.
	if len(location.Country) != 2 || location.Country[0] < 'A' || location.Country[0] > 'Z' || location.Country[1] < 'A' || location.Country[1] > 'Z' || location.Region == "" || location.City == "" || location.Timezone == "" {
		return body
	}
	for _, field := range []string{location.Region, location.City} {
		if utf8.RuneCountInString(field) > 128 || strings.IndexFunc(field, unicode.IsControl) >= 0 {
			return body
		}
	}
	if _, err := time.LoadLocation(location.Timezone); err != nil {
		return body
	}
	value := map[string]string{"type": "approximate", "country": location.Country, "region": location.Region, "city": location.City, "timezone": location.Timezone}
	for i, tool := range gjson.GetBytes(body, "tools").Array() {
		kind := tool.Get("type").String()
		if kind != "web_search" && !strings.HasPrefix(kind, "web_search_") {
			continue
		}
		if next, err := sjson.SetBytes(body, fmt.Sprintf("tools.%d.user_location", i), value); err == nil {
			body = next
		}
	}
	return body
}
