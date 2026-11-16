package platform

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"golang.org/x/crypto/bcrypt"
)

func TestTokenKindAndExpiry(t *testing.T) {
	e := &Env{JWTSecret: strings.Repeat("x", 40), Now: time.Now}
	p := Principal{ID: "player-1", ProjectID: "project-1", EnvironmentID: "environment-1", Kind: KindPlayer}
	token := e.Token(p, time.Minute)
	parsed, ok := e.ReadToken("Bearer "+token, KindPlayer)
	if !ok || parsed != p {
		t.Fatalf("valid player token rejected: %#v %v", parsed, ok)
	}
	if _, ok = e.ReadToken("Bearer "+token, KindUser); ok {
		t.Fatal("player token accepted as user token")
	}
	if _, ok = e.ReadToken("Bearer "+e.Token(p, -time.Second), KindPlayer); ok {
		t.Fatal("expired token accepted")
	}
	// The clock decides expiry, so moving it past exp rejects the token.
	e.Now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	if _, ok = e.ReadToken("Bearer "+token, KindPlayer); ok {
		t.Fatal("token accepted after the clock passed its expiry")
	}
	tampered := token[:len(token)-2] + "xx"
	if _, ok = (&Env{JWTSecret: e.JWTSecret, Now: time.Now}).ReadToken("Bearer "+tampered, KindPlayer); ok {
		t.Fatal("tampered signature accepted")
	}
}

func TestCredentialHashIsOneWayAndConstantTimeComparable(t *testing.T) {
	stored := Digest("server-secret")
	if !SecureHash(stored, "server-secret") || SecureHash(stored, "wrong") || stored == "server-secret" {
		t.Fatal("digest comparison is wrong")
	}
}

func TestRandomUUIDShape(t *testing.T) {
	if id := RandomUUID(); !UUIDPattern.MatchString(id) {
		t.Fatalf("invalid UUID: %q", id)
	}
}

func TestCheckPassword(t *testing.T) {
	hash, err := bcrypt.GenerateFromPassword([]byte("correct horse"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	if !CheckPassword(true, string(hash), "correct horse") || CheckPassword(true, string(hash), "wrong") || CheckPassword(false, "", "anything") {
		t.Fatal("password check is wrong")
	}
}

func TestIsUniqueViolation(t *testing.T) {
	if !IsUniqueViolation(fmt.Errorf("insert: %w", &pgconn.PgError{Code: "23505"})) {
		t.Fatal("wrapped unique violation not detected")
	}
	if IsUniqueViolation(&pgconn.PgError{Code: "23503"}) || IsUniqueViolation(errors.New("unique")) {
		t.Fatal("non-unique error reported as unique violation")
	}
}

func TestFieldErrorsAndInvalid(t *testing.T) {
	var errs FieldErrors
	errs.Add("items[0].key", "must be %s", "valid")
	apiErr := Invalid("INVALID_CONFIGURATION", "bad", errs)
	if apiErr.Status != 422 || errs[0].Message != "must be valid" {
		t.Fatalf("invalid error: %+v", apiErr)
	}
	var target *Error
	if !errors.As(fmt.Errorf("wrapped: %w", apiErr), &target) {
		t.Fatal("*Error does not survive wrapping")
	}
}
