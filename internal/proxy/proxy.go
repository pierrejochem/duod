// Package proxy defines the traffic switch duod drives: Traefik (file
// provider, in Docker) or Apache (include file + graceful reload).
package proxy

import (
	"context"
	"net/http"
)

// Slot is one color as the proxy sees it. An empty Name means "none".
type Slot struct {
	Name     string // "blue" | "green"
	Revision string // git sha deployed in the slot
	Port     int    // host port (apache backend only)
}

// Data is the desired routing state of one app.
type Data struct {
	App string
	// Active receives the public traffic.
	Active Slot
	// Preview is deployed and under analysis or paused.
	Preview Slot
	// Previous is kept for rollback; it only needs to be probe-able.
	Previous Slot
}

// Target is one health-check request against a slot.
type Target struct {
	Name   string // human readable, e.g. "web /health"
	URL    string
	Header http.Header
}

// Proxy is implemented by every backend.
type Proxy interface {
	// Kind is "traefik" or "apache".
	Kind() string
	// InSync reports whether the proxy configuration on disk matches d.
	InSync(d Data) (bool, error)
	// Apply writes d and returns once the proxy serves it. On error the
	// previous configuration is restored, so traffic is never left on a
	// half-applied state.
	Apply(ctx context.Context, d Data) error
	// Targets are the health checks for a slot (pre-promotion analysis,
	// active-slot monitoring and rollback).
	Targets(s Slot) []Target
	// CheckSlot verifies the wiring of a freshly started slot (e.g. that
	// its containers are attached to the proxy network).
	CheckSlot(ctx context.Context, s Slot) error
}
