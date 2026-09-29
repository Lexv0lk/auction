package httpapp

import "context"

// routeState carries the per-request observability labels that the inner
// handlers decide: the route template and the error code of a refusal. It
// lives in the request context as a mutable holder, because the response
// writer is wrapped by the CSRF middleware on unsafe methods and cannot be
// reached by a type assertion from there.
type routeState struct {
	route   string
	errCode string
}

type routeStateKey struct{}

// routeStateFrom returns the holder of the running request, if any.
func routeStateFrom(ctx context.Context) *routeState {
	state, _ := ctx.Value(routeStateKey{}).(*routeState)

	return state
}

// setRoute records the route template of the answering handler.
func (s *routeState) setRoute(route string) {
	if s != nil {
		s.route = route
	}
}

// setErrorCode records the machine-readable code of the answered refusal;
// the first code wins, so a follow-up answer never replaces it.
func (s *routeState) setErrorCode(code string) {
	if s != nil && s.errCode == "" {
		s.errCode = code
	}
}
