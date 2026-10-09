package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"
)

const (
	// IdempotencyTTL is how long a finished request's response is replayed.
	IdempotencyTTL = 24 * time.Hour
	// PendingLease is how long a claim holds while its request runs; a
	// server that died mid-request frees the key after it.
	PendingLease = 5 * time.Minute
)

// Response is what an idempotent request answered, stored for replay.
type Response struct {
	Status int
	Body   []byte
}

// Idempotency runs requests carrying an Idempotency-Key at most once per
// principal, method and path.
type Idempotency struct {
	Store IdempotencyStore
	// Now is the clock; time.Now when nil.
	Now func() time.Time
}

func (i Idempotency) now() time.Time {
	if i.Now != nil {
		return i.Now()
	}
	return time.Now()
}

// ValidKey reports whether key may be an Idempotency-Key: 1–255 printable
// ASCII characters.
func ValidKey(key string) bool {
	if key == "" || len(key) > 255 {
		return false
	}
	for i := 0; i < len(key); i++ {
		if key[i] < 0x21 || key[i] > 0x7e {
			return false
		}
	}
	return true
}

// Scope is the space a key is valid in. Another principal's key, or the
// same key on another route, never replays. The principal is length-prefixed
// so no name can run into the method, and nothing in it is a byte Postgres
// text refuses.
func Scope(principal, method, path string) string {
	return strconv.Itoa(len(principal)) + ":" + principal + " " + method + " " + path
}

// Do runs fn once for key. A repeat after fn finished gets fn's response
// again with replayed true; a repeat while fn runs gets
// ErrIdempotencyInFlight; a repeat with another body gets
// ErrIdempotencyMismatch. With no key, fn just runs.
//
// fn returns an error only when it changed nothing: the claim is then
// released so the request can be retried. Any response it returns,
// including an error status, is what repeats get.
func (i Idempotency) Do(ctx context.Context, principal, method, path, key string, body []byte,
	fn func() (Response, error)) (resp Response, replayed bool, err error) {
	if key == "" {
		resp, err = fn()
		return resp, false, err
	}
	if !ValidKey(key) {
		return Response{}, false, fmt.Errorf("%w: Idempotency-Key must be 1 to 255 printable ASCII characters", ErrInvalid)
	}
	sum := sha256.Sum256(body)
	hash := hex.EncodeToString(sum[:])
	scope := Scope(principal, method, path)
	now := i.now()
	cur, claimed, err := i.Store.Claim(ctx, IdemRecord{Scope: scope, Key: key, RequestHash: hash,
		State: IdemPending, ExpiresAt: now.Add(PendingLease)}, now)
	if err != nil {
		return Response{}, false, err
	}
	if !claimed {
		switch {
		case cur.RequestHash != hash:
			return Response{}, false, ErrIdempotencyMismatch
		case cur.State != IdemDone:
			return Response{}, false, ErrIdempotencyInFlight
		}
		return Response{Status: cur.Status, Body: cur.Body}, true, nil
	}
	resp, err = fn()
	if err != nil {
		// Released even if the caller has gone, so the key is free again.
		if rerr := i.Store.Release(context.WithoutCancel(ctx), scope, key); rerr != nil {
			err = errors.Join(err, rerr)
		}
		return Response{}, false, err
	}
	if err := i.Store.Complete(context.WithoutCancel(ctx), scope, key, resp.Status, resp.Body, i.now().Add(IdempotencyTTL)); err != nil {
		return resp, false, err
	}
	return resp, false, nil
}
