package main

import (
	"context"
	"net/http"
)

// GET /v1/users returns only the credential's member and reporting zone.

// User is one row of GET /v1/users: the users row plus what the batches log
// says about its uploads. The counts come from batches alone — one row per
// upload, cheap to aggregate — never from the sample hypertables.
type User struct {
	TimeZone  string  `json:"timeZone"`
	UserID    string  `json:"userID"`
	Name      *string `json:"name,omitempty"`
	Email     *string `json:"email,omitempty"`
	CreatedAt int64   `json:"createdAt,omitempty"`
	// Epoch milliseconds of the most recent batch; null when nothing has
	// ever been uploaded for this user.
	LastSync *int64 `json:"lastSync,omitempty"`
	// Batches received, and the samples they declared (the sum of their
	// sampleCount headers).
	Batches         int64 `json:"batches,omitempty"`
	UploadedSamples int64 `json:"uploadedSamples,omitempty"`
}

// UsersResponse is GET /v1/users.
type UsersResponse struct {
	Users []User `json:"users"`
	// The user served when a request names none (PULS_USER_ID).
	Default string `json:"default"`
	// Whether ?user= may name anyone else (PULS_MULTI_USER).
	MultiUser bool `json:"multiUser"`
	// The selected account reporting zone; each listed user carries its own.
	TimeZone string `json:"timeZone"`
}

func (st *Store) Users(ctx context.Context, member string) ([]User, error) {
	rows, err := st.pool.Query(ctx, `
		SELECT id::text, puls_user_time_zone(id) FROM users WHERE id=$1`, member)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]User, 0)
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.UserID, &u.TimeZone); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *Server) handleUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.store.Users(r.Context(), s.requestUser(r))
	if err != nil {
		s.writeStoreError(w, err, "users")
		return
	}
	def := s.requestUser(r)
	kept := make([]User, 0, 1)
	for _, u := range users {
		if sameUser(u.UserID, def) {
			kept = append(kept, User{UserID: u.UserID, TimeZone: u.TimeZone})
		}
	}
	users = kept
	zone := s.location().String()
	for _, u := range users {
		if sameUser(u.UserID, s.requestUser(r)) && u.TimeZone != "" {
			zone = u.TimeZone
		}
	}
	writeJSON(w, http.StatusOK, UsersResponse{
		Users:     users,
		Default:   def,
		MultiUser: false,
		TimeZone:  zone,
	})
}
