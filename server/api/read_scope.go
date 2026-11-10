package main

import (
	"context"
	"encoding/hex"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

type readScopeKey struct{}
type readRecheckKey struct{}

const readPageSize = 256

type readCursor struct {
	at time.Time
	id string
}

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
	ctx = context.WithValue(ctx, apiAuthorityKey{}, authority)
	return context.WithValue(ctx, readScopeKey{}, tx), closeScope, nil
}

func (st *Store) checkReadScope(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if check, ok := ctx.Value(readRecheckKey{}).(func(context.Context) error); ok {
		return check(ctx)
	}
	authority, ok := ctx.Value(apiAuthorityKey{}).(*apiAuthority)
	if !ok {
		return nil
	}
	var raw []byte
	err := st.readDB(ctx).QueryRow(ctx, "SELECT principal FROM auth.resolve_api_credential($1)", authority.TokenHash[:]).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return errAPICredential
	}
	if err != nil {
		return errAPIAuthority
	}
	var principal apiPrincipal
	if strictAuthorityJSON(raw, &principal) != nil || !principal.equals(authority.Principal) {
		return errAPICredential
	}
	return nil
}
