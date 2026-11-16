package platform

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"golang.org/x/crypto/bcrypt"
)

var (
	// KeyPattern is the shape of every stable definition key and slot key.
	KeyPattern  = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
	UUIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,48}$`)
)

// ValidSlug reports whether s is a valid project slug.
func ValidSlug(s string) bool { return slugPattern.MatchString(s) }

// RandomID returns 32 random hex characters.
func RandomID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b[:])
}

// RandomSecret returns n random bytes, Base64URL encoded.
func RandomSecret(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return RandomID()
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// RandomUUID returns a random version-4 UUID string.
func RandomUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return RandomID()
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// Digest returns the hex SHA-256 of v. Used for high-entropy secrets (credentials,
// refresh tokens) and request hashes; passwords use bcrypt instead.
func Digest(v string) string { h := sha256.Sum256([]byte(v)); return hex.EncodeToString(h[:]) }

// SecureHash compares raw against a stored digest in constant time.
func SecureHash(stored, raw string) bool {
	return subtle.ConstantTimeCompare([]byte(stored), []byte(Digest(raw))) == 1
}

// NormalizeEmail lowercases and trims an email address.
func NormalizeEmail(email string) string { return strings.ToLower(strings.TrimSpace(email)) }

// IsUniqueViolation reports whether err is a PostgreSQL unique-constraint violation.
func IsUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

var (
	dummyHashOnce sync.Once
	dummyHash     []byte
)

// BcryptCost is the cost for new password hashes (BCRYPT_COST; default 10). Stored
// hashes keep the cost they were created with. Login throughput under a surge is
// scaled by adding API instances, not by lowering this (spec 6.6).
var BcryptCost = bcrypt.DefaultCost

// HashPassword hashes a password with BcryptCost.
func HashPassword(password string) ([]byte, error) {
	return bcrypt.GenerateFromPassword([]byte(password), BcryptCost)
}

// CheckPassword compares a password against a stored bcrypt hash. When the account
// does not exist (found == false) it still runs a bcrypt comparison against a dummy
// hash, so response timing does not reveal whether an email is registered.
func CheckPassword(found bool, hash, password string) bool {
	if !found {
		dummyHashOnce.Do(func() {
			dummyHash, _ = HashPassword("realmstack-timing-equaliser")
		})
		_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(password))
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}
