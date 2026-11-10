package main

import (
	"context"
	"encoding/hex"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

type readScopeKey struct{}

type readQuery interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

func (st *Store) readDB(ctx context.Context) readQuery {
	if tx, ok := ctx.Value(readScopeKey{}).(pgx.Tx); ok {
		return tx
	}
	return st.pool
}

func (st *Store) ReadScope(ctx context.Context, authority *apiAuthority) (context.Context, func(), error) {
	tx, err := st.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadOnly})
	if err != nil {
		return ctx, nil, errAPIAuthority
	}
	closeScope := func() {
		// A cancelled request must still release its transaction and local identity.
		rollbackCtx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()
		_ = tx.Rollback(rollbackCtx)
	}
	if _, err = tx.Exec(ctx, "SELECT set_config('puls.api_credential_hash',$1,true)", hex.EncodeToString(authority.TokenHash[:])); err != nil {
		closeScope()
		return ctx, nil, errAPIAuthority
	}
	var member string
	err = tx.QueryRow(ctx, "SELECT member_id::text FROM api.read_scope()").Scan(&member)
	if err != nil || !sameUser(member, authority.Principal.MemberID) {
		closeScope()
		if errors.Is(err, pgx.ErrNoRows) || err == nil {
			return ctx, nil, errAPICredential
		}
		return ctx, nil, errAPIAuthority
	}
	return context.WithValue(ctx, readScopeKey{}, tx), closeScope, nil
}
