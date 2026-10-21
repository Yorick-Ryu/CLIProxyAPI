package helps

import (
	"bytes"
	"context"
	"crypto/sha256"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const codexTurnStateHeader = "x-codex-turn-state"

type codexTurnStateContextKey struct{}

type codexTurnStateIdentity struct {
	scope [32]byte
	owner [32]byte
}

type codexTurnStateOrigin struct {
	owner     [32]byte
	expiresAt time.Time
	ambiguous bool
}

// Store only hashes, never opaque routing tokens or caller credentials. Bound
// memory even when upstream emits a different token for every request.
type codexTurnStateStore struct {
	mu        sync.Mutex
	origins   map[[64]byte]codexTurnStateOrigin
	now       func() time.Time
	nextSweep time.Time
}

var codexTurnStates = codexTurnStateStore{now: time.Now}

const (
	codexTurnStateTTL        = time.Hour
	codexTurnStateMaxEntries = 65536
)

// WithCodexTurnState binds the original caller/thread and stable credential to
// one attempt. Account identity rewriting must not change this lookup scope.
func WithCodexTurnState(ctx context.Context, owner string, headers http.Header, body []byte, fallbackSession string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if owner == "" {
		return context.WithValue(ctx, codexTurnStateContextKey{}, struct{}{})
	}
	session := ""
	for _, name := range []string{"thread-id", "session-id", "session_id"} {
		for key, values := range headers {
			if strings.EqualFold(key, name) && len(values) > 0 {
				session = strings.TrimSpace(values[0])
				break
			}
		}
		if session != "" {
			break
		}
	}
	for _, path := range []string{"client_metadata.thread_id", "client_metadata.session_id", "prompt_cache_key"} {
		if session == "" {
			session = strings.TrimSpace(gjson.GetBytes(body, path).String())
		}
	}
	if session == "" {
		embedded := gjson.Parse(gjson.GetBytes(body, "client_metadata.x-codex-turn-metadata").String())
		for _, key := range []string{"thread_id", "session_id"} {
			if session == "" {
				session = strings.TrimSpace(embedded.Get(key).String())
			}
		}
	}
	if session == "" {
		session = strings.TrimSpace(fallbackSession)
	}
	if session == "" {
		return context.WithValue(ctx, codexTurnStateContextKey{}, struct{}{})
	}
	identity := codexTurnStateIdentity{
		scope: sha256.Sum256([]byte(APIKeyFromContext(ctx) + "\x00" + session)),
		owner: sha256.Sum256([]byte(owner)),
	}
	return context.WithValue(ctx, codexTurnStateContextKey{}, identity)
}

func codexTurnStateKey(identity codexTurnStateIdentity, token string) [64]byte {
	var key [64]byte
	copy(key[:32], identity.scope[:])
	hash := sha256.Sum256([]byte(token))
	copy(key[32:], hash[:])
	return key
}

func (s *codexTurnStateStore) foreign(identity codexTurnStateIdentity, token string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := codexTurnStateKey(identity, token)
	origin, ok := s.origins[key]
	if ok && !s.now().Before(origin.expiresAt) {
		delete(s.origins, key)
		return false
	}
	return ok && !origin.ambiguous && origin.owner != identity.owner
}

func (s *codexTurnStateStore) remember(identity codexTurnStateIdentity, token string) {
	if token == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.origins == nil {
		s.origins = make(map[[64]byte]codexTurnStateOrigin)
	}
	now := s.now()
	key := codexTurnStateKey(identity, token)
	previous, exists := s.origins[key]
	if !exists && len(s.origins) >= codexTurnStateMaxEntries {
		if !now.Before(s.nextSweep) {
			s.nextSweep = now.Add(time.Minute)
			for k, origin := range s.origins {
				if !now.Before(origin.expiresAt) {
					delete(s.origins, k)
				}
			}
		}
		if len(s.origins) >= codexTurnStateMaxEntries {
			// Unknown provenance is safer than evicting a fresh known owner.
			return
		}
	}
	ambiguous := exists && now.Before(previous.expiresAt) && (previous.ambiguous || previous.owner != identity.owner)
	s.origins[key] = codexTurnStateOrigin{owner: identity.owner, expiresAt: now.Add(codexTurnStateTTL), ambiguous: ambiguous}
}

// GuardCodexTurnStateHeaders removes only known foreign tokens, including on a
// reconnect. Unknown/expired tokens and operator headers without a scope survive.
func GuardCodexTurnStateHeaders(ctx context.Context, headers http.Header) {
	if ctx == nil {
		return
	}
	identity, ok := ctx.Value(codexTurnStateContextKey{}).(codexTurnStateIdentity)
	if !ok {
		return
	}
	for key, values := range headers {
		if !strings.EqualFold(key, codexTurnStateHeader) {
			continue
		}
		var kept []string
		for _, value := range values {
			if !codexTurnStates.foreign(identity, strings.TrimSpace(value)) {
				kept = append(kept, value)
			}
		}
		if len(kept) == 0 {
			delete(headers, key)
		} else if len(kept) != len(values) {
			headers[key] = kept
		}
	}
}

// GuardCodexTurnStateBody runs before the final payload rules; explicit payload
// overrides remain the final semantic boundary, including on WS follow-ups.
func GuardCodexTurnStateBody(ctx context.Context, body []byte) []byte {
	if ctx == nil {
		return body
	}
	identity, ok := ctx.Value(codexTurnStateContextKey{}).(codexTurnStateIdentity)
	if !ok {
		return body
	}
	metadata := gjson.GetBytes(body, "client_metadata")
	metadata.ForEach(func(key, value gjson.Result) bool {
		if strings.EqualFold(key.Str, codexTurnStateHeader) && value.Type == gjson.String && codexTurnStates.foreign(identity, strings.TrimSpace(value.Str)) {
			// The header name contains no JSON path metacharacters.
			if updated, err := sjson.DeleteBytes(body, "client_metadata."+key.Str); err == nil {
				body = updated
			}
		}
		return true
	})
	return body
}

// CodexTurnStateCommitObserver is called only with the accepted downstream
// headers/payload, after bootstrap retries and response interceptors. Reading a
// discarded upstream attempt never establishes token ownership.
func CodexTurnStateCommitObserver(ctx context.Context) func(http.Header, []byte) {
	if ctx == nil {
		return nil
	}
	identity, ok := ctx.Value(codexTurnStateContextKey{}).(codexTurnStateIdentity)
	if !ok {
		return nil
	}
	var responseHeaders sync.Once
	return func(headers http.Header, payload []byte) {
		rememberHeaders := func(values gjson.Result) {
			values.ForEach(func(key, value gjson.Result) bool {
				if strings.EqualFold(key.Str, codexTurnStateHeader) {
					if value.Type == gjson.String {
						codexTurnStates.remember(identity, strings.TrimSpace(value.Str))
					} else if value.IsArray() {
						value.ForEach(func(_, token gjson.Result) bool {
							if token.Type == gjson.String {
								codexTurnStates.remember(identity, strings.TrimSpace(token.Str))
							}
							return true
						})
					}
				}
				return true
			})
		}
		responseHeaders.Do(func() {
			for key, values := range headers {
				if strings.EqualFold(key, codexTurnStateHeader) {
					for _, token := range values {
						codexTurnStates.remember(identity, strings.TrimSpace(token))
					}
				}
			}
		})
		if !bytes.Contains(payload, []byte("codex.response.metadata")) {
			return
		}
		observeEvent := func(data []byte) {
			if !gjson.ValidBytes(data) {
				return
			}
			root := gjson.ParseBytes(data)
			if root.Get("type").String() == "codex.response.metadata" {
				rememberHeaders(root.Get("headers"))
			}
		}
		if bytes.HasPrefix(bytes.TrimSpace(payload), []byte("{")) {
			observeEvent(payload)
			return
		}
		// Executor SSE chunks contain complete data lines. Only metadata events
		// can establish body-token provenance; arbitrary text never can.
		for _, line := range bytes.Split(payload, []byte("\n")) {
			if bytes.HasPrefix(line, []byte("data:")) {
				observeEvent(bytes.TrimSpace(line[5:]))
			}
		}
	}
}
