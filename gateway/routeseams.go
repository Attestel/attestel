// routeseams.go — Wave 0 seam. Registration points for route groups that later waves add in their
// OWN files, so no two lanes edit main.go. Standard library only.
//
// A lane does NOT edit this file and does NOT edit main.go. It adds its own file with an init()
// that appends a registrar, e.g. in gateway/feeds.go (Wave 2 Lane C):
//
//	func init() {
//	    registerEventRoute(func(s *Server, mux *http.ServeMux) {
//	        mux.HandleFunc("GET /api/following", s.handleFollowing)
//	    })
//	}
//
// Today both slices are empty, so both hooks are provable no-ops: the gateway's route table is
// byte-for-byte what it was before this wave.
package main

import "net/http"

type routeRegistrar func(*Server, *http.ServeMux)

var (
	eventRouteRegistrars        []routeRegistrar
	subscriptionRouteRegistrars []routeRegistrar
)

func registerEventRoute(f routeRegistrar) { eventRouteRegistrars = append(eventRouteRegistrars, f) }
func registerSubscriptionRoute(f routeRegistrar) {
	subscriptionRouteRegistrars = append(subscriptionRouteRegistrars, f)
}

// registerEventRoutes wires the event/feed surface (Wave 2 Lane C). No-op until a registrar exists.
func (s *Server) registerEventRoutes(mux *http.ServeMux) {
	for _, f := range eventRouteRegistrars {
		f(s, mux)
	}
}

// registerSubscriptionRoutes wires the follow-graph pass-through (Wave 1 Lane A). No-op until a
// registrar exists.
func (s *Server) registerSubscriptionRoutes(mux *http.ServeMux) {
	for _, f := range subscriptionRouteRegistrars {
		f(s, mux)
	}
}
