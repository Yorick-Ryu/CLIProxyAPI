package auth

import "context"

type commitGuardContextKey struct{}
type strictPersistContextKey struct{}

// WithCredentialCommitGuard places a precondition at the persistence boundary.
// The file store evaluates it inside the credential's inter-process lock.
func WithCredentialCommitGuard(ctx context.Context, guard func(map[string]any) error) context.Context {
	return context.WithValue(context.WithValue(ctx, commitGuardContextKey{}, guard), strictPersistContextKey{}, true)
}

func credentialCommitGuard(ctx context.Context) func(map[string]any) error {
	if ctx == nil {
		return nil
	}
	guard, _ := ctx.Value(commitGuardContextKey{}).(func(map[string]any) error)
	return guard
}

func strictAuthPersistence(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	strict, _ := ctx.Value(strictPersistContextKey{}).(bool)
	return strict
}
