// Package state holds the persisted rollout status of each app.
package state

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// Phase follows Argo Rollouts terminology.
type Phase string

const (
	PhaseNew         Phase = "New"         // never deployed
	PhaseProgressing Phase = "Progressing" // preview deploying / under analysis
	PhasePaused      Phase = "Paused"      // preview healthy, waiting for promotion
	PhaseHealthy     Phase = "Healthy"     // active slot serves the desired revision
	PhaseDegraded    Phase = "Degraded"    // last rollout failed or active slot unhealthy
)

// Sync is the Argo CD style comparison between Git and the live system.
type Sync string

const (
	Synced    Sync = "Synced"
	OutOfSync Sync = "OutOfSync"
	Unknown   Sync = "Unknown"
)

// Slot is a color.
type Slot string

const (
	Blue  Slot = "blue"
	Green Slot = "green"
)

// Other returns the opposite color (blue when s is empty).
func (s Slot) Other() Slot {
	if s == Blue {
		return Green
	}
	return Blue
}

// Event is one entry of the app history.
type Event struct {
	Time     time.Time `json:"time"`
	Type     string    `json:"type"`
	Revision string    `json:"revision,omitempty"`
	Slot     Slot      `json:"slot,omitempty"`
	Message  string    `json:"message"`
}

// App is the full persisted state of one application.
type App struct {
	Name    string `json:"name"`
	Phase   Phase  `json:"phase"`
	Sync    Sync   `json:"sync"`
	Message string `json:"message"`

	TargetRevision string    `json:"targetRevision"`
	TargetSubject  string    `json:"targetSubject"`
	LastFetch      time.Time `json:"lastFetch"`
	FetchError     string    `json:"fetchError,omitempty"`

	ActiveSlot     Slot      `json:"activeSlot"`
	ActiveRevision string    `json:"activeRevision"`
	ActiveSince    time.Time `json:"activeSince"`

	PreviewSlot       Slot      `json:"previewSlot,omitempty"`
	PreviewRevision   string    `json:"previewRevision,omitempty"`
	PreviewReadySince time.Time `json:"previewReadySince,omitempty"`

	// The previous active slot is kept (running until ScaleDownAt, then
	// stopped) so `rollback` can switch back without a redeploy.
	PreviousSlot     Slot      `json:"previousSlot,omitempty"`
	PreviousRevision string    `json:"previousRevision,omitempty"`
	ScaleDownAt      time.Time `json:"scaleDownAt,omitempty"`

	// FailedRevision is not retried automatically until Git moves on or
	// someone calls `retry`.
	FailedRevision string `json:"failedRevision,omitempty"`
	FailureReason  string `json:"failureReason,omitempty"`

	ActiveHealthy  bool      `json:"activeHealthy"`
	LastSelfHeal   time.Time `json:"lastSelfHeal,omitempty"`
	ActiveFailures int       `json:"-"`

	History []Event `json:"history"`
}

const maxHistory = 50

// Record appends an event and trims the history.
func (a *App) Record(typ, rev string, slot Slot, msg string) {
	a.History = append(a.History, Event{Time: time.Now().UTC(), Type: typ, Revision: rev, Slot: slot, Message: msg})
	if len(a.History) > maxHistory {
		a.History = a.History[len(a.History)-maxHistory:]
	}
}

// Clone returns a deep copy safe to hand to API readers.
func (a *App) Clone() App {
	c := *a
	c.History = append([]Event(nil), a.History...)
	return c
}

// Store persists app states as JSON files.
type Store struct{ Dir string }

func (s Store) path(name string) string { return filepath.Join(s.Dir, name+".json") }

// Load returns the saved state or a fresh one.
func (s Store) Load(name string) (*App, error) {
	raw, err := os.ReadFile(s.path(name))
	if errors.Is(err, os.ErrNotExist) {
		return &App{Name: name, Phase: PhaseNew, Sync: Unknown}, nil
	}
	if err != nil {
		return nil, err
	}
	var a App
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, err
	}
	return &a, nil
}

// Save writes atomically.
func (s Store) Save(a *App) error {
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path(a.Name) + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path(a.Name))
}
