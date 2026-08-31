// routeseams.go — Wave 0 seam. A registration point for the subscription/event-state routes Wave 1
// Lane A adds in its OWN files, so that lane never edits handlers.go. Standard library only.
//
// Lane 1A does NOT edit this file and does NOT edit handlers.go. It adds journal/subscriptions.go
// and journal/event_state.go, each with an init() that appends a registrar:
//
//	func init() {
//	    registerSubscriptionRoute(func(s *Server, mux *http.ServeMux) {
//	        mux.HandleFunc("GET /subscriptions", s.optionalAuth(s.handleListSubscriptions))
//	        mux.HandleFunc("POST /subscriptions", s.requireAuth(s.handleCreateSubscription))
//	    })
//	}
//
// The routes that seam is reserved for are EVENT_CONTRACTS.md §2.1 (`/subscriptions`) and §2.3
// (`/event-state`). Lane 1A attached both, one init() per file, so the slice now carries two
// registrars and `routeseams_test.go` proves the routes arrive through this hook rather than from
// an edit to handlers.go. (Wave 0 shipped it empty; that sentence is kept in the past tense here
// because the file's whole purpose is to be the thing a lane appends to.)
package main

import "net/http"

type routeRegistrar func(*Server, *http.ServeMux)

var subscriptionRouteRegistrars []routeRegistrar

func registerSubscriptionRoute(f routeRegistrar) {
	subscriptionRouteRegistrars = append(subscriptionRouteRegistrars, f)
}

// registerSubscriptionRoutes wires the per-user follow graph and event state (Wave 1 Lane A).
// No-op until a registrar exists.
func (s *Server) registerSubscriptionRoutes(mux *http.ServeMux) {
	for _, f := range subscriptionRouteRegistrars {
		f(s, mux)
	}
}
