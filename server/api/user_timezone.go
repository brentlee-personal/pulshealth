package main

import (
	"context"
	"fmt"
	"time"
)

type userLocationKey struct{}
type userLocation struct {
	userID string
	loc    *time.Location
}

// UserLocation resolves the selected account without mutating shared store
// state. A summary passes its resolved zone to nested calendar reads through
// context, avoiding repeated lookups for each section.
func (st *Store) UserLocation(ctx context.Context, userID string) (*time.Location, error) {
	if scoped, ok := ctx.Value(userLocationKey{}).(userLocation); ok && sameUser(scoped.userID, userID) {
		return scoped.loc, nil
	}
	var name string
	if err := st.readDB(ctx).QueryRow(ctx, `SELECT puls_user_time_zone($1::uuid)`, userID).Scan(&name); err != nil {
		return nil, fmt.Errorf("account time zone: %w", err)
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("account time zone %q: %w", name, err)
	}
	return loc, nil
}

func summaryLocation(name string, fallback *time.Location) *time.Location {
	if name != "" {
		if loc, err := time.LoadLocation(name); err == nil {
			return loc
		}
	}
	return fallback
}
