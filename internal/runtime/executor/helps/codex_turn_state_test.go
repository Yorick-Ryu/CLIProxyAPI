package helps

import (
	"context"
	"crypto/sha256"
	"fmt"
	"github.com/gin-gonic/gin"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/tidwall/gjson"
)

func TestCodexTurnStateCommittedProvenance(t *testing.T) {
	headers := http.Header{"Thread-Id": {t.Name()}}
	a := WithCodexTurnState(context.Background(), "synthetic-account-a", headers, nil, "")
	b := WithCodexTurnState(context.Background(), "synthetic-account-b", headers, nil, "")
	token := t.Name() + "-opaque-state"
	guard := func(ctx context.Context, want bool) {
		h := http.Header{"x-CoDeX-TuRn-StAtE": {token}, "Other": {"keep"}}
		GuardCodexTurnStateHeaders(ctx, h)
		if (len(h["x-CoDeX-TuRn-StAtE"]) > 0) != want || h.Get("Other") != "keep" {
			t.Fatalf("header guard retained token = %t, want %t", len(h["x-CoDeX-TuRn-StAtE"]) > 0, want)
		}
		body := []byte(fmt.Sprintf(`{"client_metadata":{"X-Codex-Turn-State":%q,"unknown":"keep"}}`, token))
		guarded := GuardCodexTurnStateBody(ctx, body)
		if gjson.GetBytes(guarded, "client_metadata.X-Codex-Turn-State").Exists() != want || gjson.GetBytes(guarded, "client_metadata.unknown").String() != "keep" {
			t.Fatal("body guard changed unrelated metadata or retained foreign token")
		}
	}
	// Merely preparing an observer or reading an upstream response cannot
	// establish provenance. The selected downstream response must commit it.
	observer := CodexTurnStateCommitObserver(a)
	guard(b, true)
	observer(http.Header{"X-Codex-Turn-State": {token}}, nil)
	guard(a, true)
	guard(b, false)
	otherThread := WithCodexTurnState(context.Background(), "synthetic-account-b", http.Header{"Thread-Id": {t.Name() + "-other"}}, nil, "")
	guard(otherThread, true)
	// The same stable credential keeps ownership across a refreshed context.
	guard(WithCodexTurnState(context.Background(), "synthetic-account-a", headers, nil, ""), true)
	// Explicitly entering an API-key/public-API path removes an inherited scope.
	guard(WithCodexTurnState(b, "", headers, nil, ""), true)

	for _, sse := range []bool{false, true} {
		state := fmt.Sprintf("%s-metadata-%t", t.Name(), sse)
		payload := []byte(fmt.Sprintf(`{"type":"codex.response.metadata","headers":{"X-Codex-Turn-State":%q}}`, state))
		if sse {
			payload = append(append([]byte("data: "), payload...), '\n', '\n')
		}
		observer(nil, payload)
		h := http.Header{"X-Codex-Turn-State": {state}}
		GuardCodexTurnStateHeaders(b, h)
		if h.Get("X-Codex-Turn-State") != "" {
			t.Fatal("committed metadata event did not establish provenance")
		}
	}
}

func TestCodexTurnStateExpiryBoundsAndAmbiguity(t *testing.T) {
	now := time.Unix(1700000000, 0)
	store := codexTurnStateStore{now: func() time.Time { return now }}
	a := codexTurnStateIdentity{scope: sha256.Sum256([]byte("caller/thread")), owner: sha256.Sum256([]byte("account-a"))}
	b := a
	b.owner = sha256.Sum256([]byte("account-b"))
	store.remember(a, "state")
	if !store.foreign(b, "state") || store.foreign(a, "state") || store.foreign(b, "unknown") {
		t.Fatal("incorrect known/unknown ownership")
	}
	now = now.Add(codexTurnStateTTL)
	if store.foreign(b, "state") {
		t.Fatal("expired token retained ownership")
	}
	store.remember(a, "shared")
	store.remember(b, "shared")
	if store.foreign(a, "shared") || store.foreign(b, "shared") {
		t.Fatal("a token emitted by multiple owners must remain ambiguous")
	}
	for n := 0; n < codexTurnStateMaxEntries+10; n++ {
		store.remember(a, fmt.Sprint(n))
	}
	if len(store.origins) != codexTurnStateMaxEntries {
		t.Fatal("provenance table is not bounded")
	}
	now = now.Add(codexTurnStateTTL + time.Minute)
	store.remember(a, "fresh")
	if !store.foreign(b, "fresh") || len(store.origins) != 1 {
		t.Fatal("expired entries did not make room for new provenance")
	}
}

func TestCodexTurnStateConcurrentCommitAndGuard(t *testing.T) {
	ctx := WithCodexTurnState(context.Background(), "owner", http.Header{"Session-Id": {t.Name()}}, nil, "")
	observer := CodexTurnStateCommitObserver(ctx)
	var wg sync.WaitGroup
	for n := 0; n < 16; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				h := http.Header{"X-Codex-Turn-State": {"synthetic-token"}}
				observer(h, nil)
				GuardCodexTurnStateHeaders(ctx, h)
			}
		}()
	}
	wg.Wait()
}

func TestCodexTurnStateCallerIsolationAndUnknownInput(t *testing.T) {
	makeContext := func(caller, owner string) context.Context {
		recorder := httptest.NewRecorder()
		ginCtx, _ := gin.CreateTestContext(recorder)
		ginCtx.Set("userApiKey", caller)
		return WithCodexTurnState(context.WithValue(context.Background(), "gin", ginCtx), owner, http.Header{"Session-Id": {t.Name()}}, nil, "")
	}
	a := makeContext("caller-a", "account-a")
	b := makeContext("caller-b", "account-b")
	observer := CodexTurnStateCommitObserver(a)
	observer(http.Header{"X-Codex-Turn-State": {"caller-state"}}, nil)
	h := http.Header{"X-Codex-Turn-State": {"caller-state"}}
	GuardCodexTurnStateHeaders(b, h)
	if h.Get("X-Codex-Turn-State") == "" {
		t.Fatal("another caller's provenance must not affect this request")
	}
	for _, payload := range []string{`{"type":"response.output_text.delta","delta":"codex.response.metadata","headers":{"x-codex-turn-state":"untrusted"}}`, `data: {invalid codex.response.metadata}`} {
		observer(nil, []byte(payload))
	}
	sameCallerOtherOwner := makeContext("caller-a", "account-b")
	h = http.Header{"X-Codex-Turn-State": {"untrusted"}}
	GuardCodexTurnStateHeaders(sameCallerOtherOwner, h)
	if h.Get("X-Codex-Turn-State") == "" {
		t.Fatal("non-metadata output established provenance")
	}
}
