package app

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"
)

// rateLimiter is a fixed-window counter per key. It is per process: with several API
// instances each enforces its own budget (a shared store is planned for Phase 8).
type rateLimiter struct {
	mu      sync.Mutex
	limit   int
	window  time.Duration
	entries map[string]limitEntry
}

type limitEntry struct {
	count int
	reset time.Time
}

func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	return &rateLimiter{limit: limit, window: window, entries: map[string]limitEntry{}}
}

// allow records one attempt for key and reports whether it is within the limit,
// plus the time until the current window resets.
func (l *rateLimiter) allow(key string, now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	entry := l.entries[key]
	if !now.Before(entry.reset) {
		entry = limitEntry{reset: now.Add(l.window)}
	}
	entry.count++
	l.entries[key] = entry
	if len(l.entries) > 10000 {
		for k, v := range l.entries {
			if !now.Before(v.reset) {
				delete(l.entries, k)
			}
		}
	}
	return entry.count <= l.limit, entry.reset.Sub(now)
}

func (s *Server) rateLimit() gin.HandlerFunc {
	return func(c *gin.Context) {
		// ClientIP honours X-Forwarded-For only from Config.TrustedProxies.
		allowed, retry := s.limiter.allow(c.ClientIP()+"|"+c.FullPath(), time.Now())
		if !allowed {
			c.Header("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
			failure(c, 429, "RATE_LIMITED", "Too many authentication attempts; try again shortly.")
			return
		}
		c.Next()
	}
}

// defaultDisplayName never derives from the email address, because display names are
// public on leaderboards.
func defaultDisplayName() string { return "Player-" + strings.ToUpper(randomID()[:6]) }

func (s *Server) resolvePublic(c *gin.Context, public string) (string, string, bool) {
	if public == "" {
		public = c.GetHeader("X-Public-Environment")
	}
	p, ok := s.lookupPublic(c, public)
	return p.ProjectID, p.EnvironmentID, ok
}

func (s *Server) registerPlayer(c *gin.Context) {
	var in struct {
		PublicEnvironment string `json:"public_environment_id"`
		EnvironmentID     string `json:"environment_id"`
		Email             string `json:"email"`
		Password          string `json:"password"`
		DisplayName       string `json:"display_name"`
	}
	if !bind(c, &in) {
		return
	}
	public := in.PublicEnvironment
	if public == "" {
		public = in.EnvironmentID
	}
	projectID, envID, ok := s.resolvePublic(c, public)
	if !ok {
		return
	}
	email := normalizeEmail(in.Email)
	if len(email) < 5 || len(email) > 254 || len(in.Password) < 8 || len(in.Password) > 128 {
		failure(c, 422, "VALIDATION_ERROR", "Email or password is invalid.")
		return
	}
	display := strings.TrimSpace(in.DisplayName)
	if display == "" {
		display = defaultDisplayName()
	}
	if len(display) > 80 {
		failure(c, 422, "VALIDATION_ERROR", "Display name is too long.")
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(in.Password), bcrypt.DefaultCost)
	if err != nil {
		internal(c, err)
		return
	}
	// external_player_key is reserved for trusted-server linking; clients cannot set it,
	// so nobody can claim another person's external identity at registration.
	var id string
	err = s.db.QueryRow(c, `INSERT INTO players(project_id,environment_id,email,password_hash,display_name) VALUES($1,$2,$3,$4,$5) RETURNING id::text`, projectID, envID, email, string(hash), display).Scan(&id)
	if err != nil {
		if isUniqueViolation(err) {
			failure(c, 409, "CONFLICT", "A player with that email already exists.")
			return
		}
		internal(c, err)
		return
	}
	access, refresh, err := s.newPlayerTokens(c, id, projectID, envID)
	if err != nil {
		internal(c, err)
		return
	}
	success(c, 201, gin.H{"player": gin.H{"id": id, "email": email, "display_name": display}, "access_token": access, "refresh_token": refresh, "token_type": "Bearer", "expires_in": int(s.cfg.AccessTTL.Seconds())})
}

func (s *Server) loginPlayer(c *gin.Context) {
	var in struct {
		PublicEnvironment string `json:"public_environment_id"`
		EnvironmentID     string `json:"environment_id"`
		Email             string `json:"email"`
		Password          string `json:"password"`
	}
	if !bind(c, &in) {
		return
	}
	public := in.PublicEnvironment
	if public == "" {
		public = in.EnvironmentID
	}
	projectID, envID, ok := s.resolvePublic(c, public)
	if !ok {
		return
	}
	var id, hash, display string
	err := s.db.QueryRow(c, `SELECT id::text,COALESCE(password_hash,''),display_name FROM players WHERE project_id=$1 AND environment_id=$2 AND email=$3`, projectID, envID, normalizeEmail(in.Email)).Scan(&id, &hash, &display)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		internal(c, err)
		return
	}
	if !checkPassword(err == nil && hash != "", hash, in.Password) {
		failure(c, 401, "INVALID_CREDENTIALS", "Email or password is incorrect.")
		return
	}
	access, refresh, err := s.newPlayerTokens(c, id, projectID, envID)
	if err != nil {
		internal(c, err)
		return
	}
	success(c, 200, gin.H{"player": gin.H{"id": id, "display_name": display}, "access_token": access, "refresh_token": refresh, "token_type": "Bearer", "expires_in": int(s.cfg.AccessTTL.Seconds())})
}

func (s *Server) newPlayerTokens(c *gin.Context, id, p, e string) (string, string, error) {
	raw := randomSecret(48)
	family := randomUUID()
	_, err := s.db.Exec(c, `INSERT INTO refresh_tokens(player_id,family_id,token_hash,expires_at) VALUES($1,$2,$3,$4)`, id, family, digest(raw), s.now().Add(s.cfg.RefreshTTL))
	if err != nil {
		return "", "", err
	}
	return s.token(principal{ID: id, ProjectID: p, EnvironmentID: e, Kind: "player"}, s.cfg.AccessTTL), raw, nil
}

func (s *Server) refreshPlayer(c *gin.Context) {
	var in struct {
		RefreshToken string `json:"refresh_token"`
	}
	if !bind(c, &in) {
		return
	}
	if len(in.RefreshToken) < 20 {
		failure(c, 401, "INVALID_REFRESH_TOKEN", "Refresh token is invalid.")
		return
	}
	tx, err := s.db.Begin(c)
	if err != nil {
		internal(c, err)
		return
	}
	defer tx.Rollback(c)
	var tokenID, playerID, family, p, e string
	var revoked *time.Time
	var expires time.Time
	err = tx.QueryRow(c, `SELECT rt.id::text,rt.player_id::text,rt.family_id,rt.revoked_at,rt.expires_at,p.project_id::text,p.environment_id::text FROM refresh_tokens rt JOIN players p ON p.id=rt.player_id JOIN projects pr ON pr.id=p.project_id AND pr.status='active' WHERE rt.token_hash=$1 FOR UPDATE OF rt`, digest(in.RefreshToken)).Scan(&tokenID, &playerID, &family, &revoked, &expires, &p, &e)
	if err != nil {
		failure(c, 401, "INVALID_REFRESH_TOKEN", "Refresh token is invalid or expired.")
		return
	}
	if revoked != nil {
		_, _ = tx.Exec(c, `UPDATE refresh_tokens SET revoked_at=COALESCE(revoked_at,now()) WHERE family_id=$1`, family)
		_ = tx.Commit(c)
		failure(c, 401, "REFRESH_TOKEN_REUSE", "Refresh token reuse detected; the token family was revoked.")
		return
	}
	if !s.now().Before(expires) {
		failure(c, 401, "INVALID_REFRESH_TOKEN", "Refresh token is expired.")
		return
	}
	raw := randomSecret(48)
	var newID string
	err = tx.QueryRow(c, `INSERT INTO refresh_tokens(player_id,family_id,token_hash,expires_at) VALUES($1,$2,$3,$4) RETURNING id::text`, playerID, family, digest(raw), s.now().Add(s.cfg.RefreshTTL)).Scan(&newID)
	if err != nil {
		internal(c, err)
		return
	}
	_, err = tx.Exec(c, `UPDATE refresh_tokens SET revoked_at=now(),replaced_by=$1 WHERE id=$2`, newID, tokenID)
	if err != nil {
		internal(c, err)
		return
	}
	if err = tx.Commit(c); err != nil {
		internal(c, err)
		return
	}
	success(c, 200, gin.H{"access_token": s.token(principal{ID: playerID, ProjectID: p, EnvironmentID: e, Kind: "player"}, s.cfg.AccessTTL), "refresh_token": raw, "token_type": "Bearer", "expires_in": int(s.cfg.AccessTTL.Seconds())})
}

func (s *Server) getMe(c *gin.Context) {
	p := getPrincipal(c)
	var id, email, display string
	var metadata []byte
	err := s.db.QueryRow(c, `SELECT id::text,COALESCE(email,''),display_name,metadata FROM players WHERE id=$1 AND project_id=$2 AND environment_id=$3`, p.ID, p.ProjectID, p.EnvironmentID).Scan(&id, &email, &display, &metadata)
	if err != nil {
		notFound(c)
		return
	}
	success(c, 200, gin.H{"id": id, "email": email, "display_name": display, "metadata": json.RawMessage(metadata)})
}
func (s *Server) patchMe(c *gin.Context) {
	var in struct {
		DisplayName *string        `json:"display_name"`
		Metadata    map[string]any `json:"metadata"`
	}
	if !bind(c, &in) {
		return
	}
	if in.DisplayName == nil && in.Metadata == nil {
		failure(c, 422, "VALIDATION_ERROR", "Provide display_name or metadata.")
		return
	}
	if in.DisplayName != nil && (strings.TrimSpace(*in.DisplayName) == "" || len(*in.DisplayName) > 80) {
		failure(c, 422, "VALIDATION_ERROR", "Display name is invalid.")
		return
	}
	p := getPrincipal(c)
	meta := []byte("null")
	if in.Metadata != nil {
		var err error
		meta, err = json.Marshal(in.Metadata)
		if err != nil || len(meta) > 16384 {
			failure(c, 422, "VALIDATION_ERROR", "Metadata is invalid or too large.")
			return
		}
	}
	key := getIdempotencyKey(c)
	tx, err := s.db.Begin(c)
	if err != nil {
		internal(c, err)
		return
	}
	defer tx.Rollback(c)
	record, replayStatus, ok := s.idempotentStart(c, tx, p, key, c.FullPath(), in)
	if !ok {
		return
	}
	if replayStatus != 0 {
		_ = tx.Rollback(c)
		success(c, replayStatus, record)
		return
	}
	_, err = tx.Exec(c, `UPDATE players SET display_name=COALESCE($1,display_name),metadata=CASE WHEN $2::jsonb='null'::jsonb THEN metadata ELSE $2::jsonb END WHERE id=$3 AND project_id=$4 AND environment_id=$5`, in.DisplayName, meta, p.ID, p.ProjectID, p.EnvironmentID)
	if err != nil {
		internal(c, err)
		return
	}
	var currentEmail, currentName string
	var currentMeta []byte
	err = tx.QueryRow(c, `SELECT COALESCE(email,''),display_name,metadata FROM players WHERE id=$1 AND project_id=$2 AND environment_id=$3`, p.ID, p.ProjectID, p.EnvironmentID).Scan(&currentEmail, &currentName, &currentMeta)
	if err != nil {
		notFound(c)
		return
	}
	result := gin.H{"id": p.ID, "email": currentEmail, "display_name": currentName, "metadata": json.RawMessage(currentMeta)}
	if err = s.idempotentSave(c, tx, p, key, record, 200, result); err != nil {
		internal(c, err)
		return
	}
	if err = tx.Commit(c); err != nil {
		internal(c, err)
		return
	}
	success(c, 200, result)
}

func getIdempotencyKey(c *gin.Context) string {
	key := strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	if len(key) < 1 || len(key) > 200 {
		return ""
	}
	return key
}

// idempotentStart locks the (actor kind, actor, key) triple and checks for a previous
// result. The kind namespaces keys: a player's self-service keys and the trusted
// server's keys for that player (actor.ID is the player in both cases) never collide.
// The hash covers route, actor and payload but not which server credential was used,
// so a retry after credential rotation replays instead of conflicting.
// It returns the stored result and its original HTTP status when the request is a
// replay (replayStatus != 0), or a marker to pass to idempotentSave otherwise.
func (s *Server) idempotentStart(c *gin.Context, tx pgx.Tx, actor principal, key, route string, payload any) (record json.RawMessage, replayStatus int, ok bool) {
	if key == "" {
		failure(c, 400, "IDEMPOTENCY_KEY_REQUIRED", "Idempotency-Key header is required.")
		return nil, 0, false
	}
	kind := idempotencyKind(actor)
	if _, err := tx.Exec(c, `SELECT pg_advisory_xact_lock(hashtext($1))`, actor.ProjectID+":"+actor.EnvironmentID+":"+kind+":"+actor.ID+":"+key); err != nil {
		internal(c, err)
		return nil, 0, false
	}
	body, _ := json.Marshal(gin.H{"route": route, "actor": actor.ID, "kind": kind, "payload": payload})
	hash := digest(string(body))
	var raw []byte
	var prior string
	var status int
	err := tx.QueryRow(c, `SELECT request_hash,result,status_code FROM idempotency_records WHERE project_id=$1 AND environment_id=$2 AND actor_kind=$3 AND actor_id=$4 AND key=$5 FOR UPDATE`, actor.ProjectID, actor.EnvironmentID, kind, actor.ID, key).Scan(&prior, &raw, &status)
	if err == nil {
		if prior != hash {
			failure(c, 409, "IDEMPOTENCY_CONFLICT", "Idempotency key was already used with a different request.")
			return nil, 0, false
		}
		return raw, status, true
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		internal(c, err)
		return nil, 0, false
	}
	return json.RawMessage(`{"_hash":"` + hash + `"}`), 0, true
}

// idempotencyKind is the idempotency namespace for an actor: "server" for trusted
// server credentials, "player" otherwise.
func idempotencyKind(actor principal) string {
	if actor.Kind == "server" {
		return "server"
	}
	return "player"
}

// idempotentSave stores the result and HTTP status so a replay returns both unchanged.
func (s *Server) idempotentSave(c *gin.Context, tx pgx.Tx, actor principal, key string, record json.RawMessage, status int, result any) error {
	var marker struct {
		Hash string `json:"_hash"`
	}
	_ = json.Unmarshal(record, &marker)
	b, err := json.Marshal(result)
	if err != nil {
		return err
	}
	_, err = tx.Exec(c, `INSERT INTO idempotency_records(project_id,environment_id,actor_kind,actor_id,key,request_hash,result,status_code) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, actor.ProjectID, actor.EnvironmentID, idempotencyKind(actor), actor.ID, key, marker.Hash, b, status)
	return err
}
func (s *Server) getInventory(c *gin.Context) {
	p := getPrincipal(c)
	rows, err := s.db.Query(c, `SELECT d.item_key,d.name,d.category,i.quantity,d.metadata,COALESCE(e.slot_key,'') FROM inventory i JOIN item_definitions d ON d.id=i.item_id LEFT JOIN equipment e ON e.player_id=i.player_id AND e.item_id=i.item_id WHERE i.player_id=$1 AND i.project_id=$2 AND i.environment_id=$3 AND i.quantity>0 ORDER BY d.item_key`, p.ID, p.ProjectID, p.EnvironmentID)
	if err != nil {
		internal(c, err)
		return
	}
	defer rows.Close()
	out := []gin.H{}
	for rows.Next() {
		var key, name, cat, slot string
		var qty int64
		var meta []byte
		if err = rows.Scan(&key, &name, &cat, &qty, &meta, &slot); err != nil {
			internal(c, err)
			return
		}
		out = append(out, gin.H{"item_key": key, "name": name, "category": cat, "quantity": qty, "metadata": json.RawMessage(meta), "equipped_slot": slot})
	}
	if err = rows.Err(); err != nil {
		internal(c, err)
		return
	}
	success(c, 200, out)
}
func (s *Server) getInventoryItem(c *gin.Context) {
	p := getPrincipal(c)
	var name, cat, slot string
	var qty int64
	var meta []byte
	err := s.db.QueryRow(c, `SELECT d.name,d.category,i.quantity,d.metadata,COALESCE(e.slot_key,'') FROM inventory i JOIN item_definitions d ON d.id=i.item_id LEFT JOIN equipment e ON e.player_id=i.player_id AND e.item_id=i.item_id WHERE i.player_id=$1 AND i.project_id=$2 AND i.environment_id=$3 AND d.item_key=$4`, p.ID, p.ProjectID, p.EnvironmentID, c.Param("itemKey")).Scan(&name, &cat, &qty, &meta, &slot)
	if err != nil {
		notFound(c)
		return
	}
	success(c, 200, gin.H{"item_key": c.Param("itemKey"), "name": name, "category": cat, "quantity": qty, "metadata": json.RawMessage(meta), "equipped_slot": slot})
}

type itemMutation struct {
	ItemKey   string `json:"item_key"`
	Quantity  int64  `json:"quantity"`
	Reason    string `json:"reason"`
	Reference string `json:"reference"`
}

func (s *Server) consumeItem(c *gin.Context) {
	var in itemMutation
	if !bind(c, &in) {
		return
	}
	if in.Quantity <= 0 || in.Quantity > 1e9 {
		failure(c, 422, "VALIDATION_ERROR", "Quantity must be positive.")
		return
	}
	p := getPrincipal(c)
	key := getIdempotencyKey(c)
	tx, err := s.db.Begin(c)
	if err != nil {
		internal(c, err)
		return
	}
	defer tx.Rollback(c)
	record, replayStatus, ok := s.idempotentStart(c, tx, p, key, c.FullPath(), in)
	if !ok {
		return
	}
	if replayStatus != 0 {
		_ = tx.Rollback(c)
		success(c, replayStatus, record)
		return
	}
	var itemID string
	var qty int64
	err = tx.QueryRow(c, `SELECT i.item_id::text,i.quantity FROM inventory i JOIN item_definitions d ON d.id=i.item_id WHERE i.player_id=$1 AND i.project_id=$2 AND i.environment_id=$3 AND d.item_key=$4 FOR UPDATE OF i`, p.ID, p.ProjectID, p.EnvironmentID, in.ItemKey).Scan(&itemID, &qty)
	if err != nil {
		notFound(c)
		return
	}
	if qty < in.Quantity {
		failure(c, 422, "INSUFFICIENT_ITEMS", "The player does not own enough items.")
		return
	}
	after := qty - in.Quantity
	_, err = tx.Exec(c, `UPDATE inventory SET quantity=$1,updated_at=now() WHERE player_id=$2 AND item_id=$3`, after, p.ID, itemID)
	if err != nil {
		internal(c, err)
		return
	}
	unequipped := false
	if after == 0 {
		// An item the player no longer owns cannot stay equipped.
		tag, delErr := tx.Exec(c, `DELETE FROM equipment WHERE player_id=$1 AND item_id=$2`, p.ID, itemID)
		if delErr != nil {
			internal(c, delErr)
			return
		}
		unequipped = tag.RowsAffected() > 0
	}
	_, err = tx.Exec(c, `INSERT INTO economy_transactions(player_id,project_id,environment_id,item_id,operation,quantity,reason,reference) VALUES($1,$2,$3,$4,'consume',$5,$6,$7)`, p.ID, p.ProjectID, p.EnvironmentID, itemID, in.Quantity, in.Reason, in.Reference)
	if err != nil {
		internal(c, err)
		return
	}
	result := gin.H{"item_key": in.ItemKey, "consumed": in.Quantity, "remaining": after, "unequipped": unequipped}
	if err = s.idempotentSave(c, tx, p, key, record, 200, result); err != nil {
		internal(c, err)
		return
	}
	if err = tx.Commit(c); err != nil {
		internal(c, err)
		return
	}
	success(c, 200, result)
}
func (s *Server) equipItem(c *gin.Context) {
	var in struct {
		ItemKey string `json:"item_key"`
		Slot    string `json:"slot"`
	}
	if !bind(c, &in) {
		return
	}
	if !keyPattern.MatchString(in.Slot) {
		failure(c, 422, "VALIDATION_ERROR", "A valid slot is required.")
		return
	}
	p := getPrincipal(c)
	key := getIdempotencyKey(c)
	tx, err := s.db.Begin(c)
	if err != nil {
		internal(c, err)
		return
	}
	defer tx.Rollback(c)
	record, replayStatus, ok := s.idempotentStart(c, tx, p, key, c.FullPath(), in)
	if !ok {
		return
	}
	if replayStatus != 0 {
		_ = tx.Rollback(c)
		success(c, replayStatus, record)
		return
	}
	var item string
	var rawMeta []byte
	err = tx.QueryRow(c, `SELECT i.item_id::text,d.metadata FROM inventory i JOIN item_definitions d ON d.id=i.item_id WHERE i.player_id=$1 AND i.project_id=$2 AND i.environment_id=$3 AND d.item_key=$4 AND i.quantity>0 FOR UPDATE OF i`, p.ID, p.ProjectID, p.EnvironmentID, in.ItemKey).Scan(&item, &rawMeta)
	if err != nil {
		failure(c, 422, "ITEM_NOT_OWNED", "The player does not own that item.")
		return
	}
	// The item's metadata.slot is its only valid slot. Because each item has exactly
	// one slot, the same stack can never be equipped in two slots at once.
	var meta map[string]any
	_ = json.Unmarshal(rawMeta, &meta)
	slot, equippable := itemSlot(meta)
	if !equippable {
		failure(c, 422, "ITEM_NOT_EQUIPPABLE", "That item cannot be equipped.")
		return
	}
	if slot != in.Slot {
		failure(c, 422, "INVALID_SLOT", fmt.Sprintf("That item can only be equipped in the %q slot.", slot))
		return
	}
	_, err = tx.Exec(c, `INSERT INTO equipment(player_id,project_id,environment_id,slot_key,item_id) VALUES($1,$2,$3,$4,$5) ON CONFLICT(player_id,slot_key) DO UPDATE SET item_id=EXCLUDED.item_id`, p.ID, p.ProjectID, p.EnvironmentID, in.Slot, item)
	if err != nil {
		internal(c, err)
		return
	}
	result := gin.H{"slot": in.Slot, "item_key": in.ItemKey}
	if err = s.idempotentSave(c, tx, p, key, record, 200, result); err != nil {
		internal(c, err)
		return
	}
	if err = tx.Commit(c); err != nil {
		internal(c, err)
		return
	}
	success(c, 200, result)
}
func (s *Server) unequipItem(c *gin.Context) {
	var in struct {
		Slot string `json:"slot"`
	}
	if !bind(c, &in) {
		return
	}
	p := getPrincipal(c)
	if !keyPattern.MatchString(in.Slot) {
		failure(c, 422, "VALIDATION_ERROR", "A valid slot is required.")
		return
	}
	key := getIdempotencyKey(c)
	tx, err := s.db.Begin(c)
	if err != nil {
		internal(c, err)
		return
	}
	defer tx.Rollback(c)
	record, replayStatus, ok := s.idempotentStart(c, tx, p, key, c.FullPath(), in)
	if !ok {
		return
	}
	if replayStatus != 0 {
		_ = tx.Rollback(c)
		success(c, replayStatus, record)
		return
	}
	tag, err := tx.Exec(c, `DELETE FROM equipment WHERE player_id=$1 AND project_id=$2 AND environment_id=$3 AND slot_key=$4`, p.ID, p.ProjectID, p.EnvironmentID, in.Slot)
	if err != nil {
		internal(c, err)
		return
	}
	if tag.RowsAffected() == 0 {
		notFound(c)
		return
	}
	result := gin.H{"slot": in.Slot, "equipped": false}
	if err = s.idempotentSave(c, tx, p, key, record, 200, result); err != nil {
		internal(c, err)
		return
	}
	if err = tx.Commit(c); err != nil {
		internal(c, err)
		return
	}
	success(c, 200, result)
}

// maxBalance caps any balance well inside the int64 range.
const maxBalance = 9_000_000_000_000_000

type currencyMutation struct {
	CurrencyKey string `json:"currency_key"`
	Amount      int64  `json:"amount"`
	Reason      string `json:"reason"`
	Reference   string `json:"reference"`
}

func (s *Server) getBalances(c *gin.Context) {
	p := getPrincipal(c)
	rows, err := s.db.Query(c, `SELECT d.currency_key,d.name,COALESCE(b.amount,0) FROM currency_definitions d LEFT JOIN balances b ON b.currency_id=d.id AND b.player_id=$1 WHERE d.project_id=$2 AND d.environment_id=$3 AND d.active ORDER BY d.currency_key`, p.ID, p.ProjectID, p.EnvironmentID)
	if err != nil {
		internal(c, err)
		return
	}
	defer rows.Close()
	out := []gin.H{}
	for rows.Next() {
		var key, name string
		var amount int64
		if err = rows.Scan(&key, &name, &amount); err != nil {
			internal(c, err)
			return
		}
		out = append(out, gin.H{"currency_key": key, "name": name, "balance": amount})
	}
	if err = rows.Err(); err != nil {
		internal(c, err)
		return
	}
	success(c, 200, out)
}
func (s *Server) spendCurrency(c *gin.Context) {
	var in currencyMutation
	if !bind(c, &in) {
		return
	}
	if in.Amount <= 0 {
		failure(c, 422, "VALIDATION_ERROR", "Amount must be positive.")
		return
	}
	p := getPrincipal(c)
	s.applyCurrency(c, p, p.ID, in, "spend")
}
func (s *Server) creditCurrency(c *gin.Context) {
	var in currencyMutation
	if !bind(c, &in) {
		return
	}
	p := getPrincipal(c)
	s.applyCurrency(c, p, c.Param("playerId"), in, "credit")
}
func (s *Server) applyCurrency(c *gin.Context, p principal, playerID string, in currencyMutation, operation string) {
	if in.Amount <= 0 || in.Amount > 1e12 {
		failure(c, 422, "VALIDATION_ERROR", "Amount must be positive and within limits.")
		return
	}
	if !s.playerInScope(c, p, playerID) {
		notFound(c)
		return
	}
	key := getIdempotencyKey(c)
	tx, err := s.db.Begin(c)
	if err != nil {
		internal(c, err)
		return
	}
	defer tx.Rollback(c)
	actor := p
	actor.ID = playerID
	record, replayStatus, ok := s.idempotentStart(c, tx, actor, key, c.FullPath(), in)
	if !ok {
		return
	}
	if replayStatus != 0 {
		_ = tx.Rollback(c)
		success(c, replayStatus, record)
		return
	}
	var currencyID string
	err = tx.QueryRow(c, `SELECT id::text FROM currency_definitions WHERE project_id=$1 AND environment_id=$2 AND currency_key=$3 AND active`, p.ProjectID, p.EnvironmentID, in.CurrencyKey).Scan(&currencyID)
	if err != nil {
		notFound(c)
		return
	}
	// One atomic statement per operation: the row lock taken by UPDATE / ON CONFLICT
	// serializes concurrent writers, and the WHERE clause is re-checked after the lock.
	var after int64
	if operation == "spend" {
		err = tx.QueryRow(c, `UPDATE balances SET amount=amount-$3,updated_at=now() WHERE player_id=$1 AND currency_id=$2 AND amount>=$3 RETURNING amount`, playerID, currencyID, in.Amount).Scan(&after)
		if errors.Is(err, pgx.ErrNoRows) {
			failure(c, 422, "INSUFFICIENT_FUNDS", "The player does not have enough currency.")
			return
		}
	} else {
		err = tx.QueryRow(c, `INSERT INTO balances(player_id,project_id,environment_id,currency_id,amount) VALUES($1,$2,$3,$4,$5) ON CONFLICT(player_id,currency_id) DO UPDATE SET amount=balances.amount+EXCLUDED.amount,updated_at=now() WHERE balances.amount+EXCLUDED.amount<=$6 RETURNING amount`, playerID, p.ProjectID, p.EnvironmentID, currencyID, in.Amount, maxBalance).Scan(&after)
		if errors.Is(err, pgx.ErrNoRows) {
			failure(c, 422, "BALANCE_LIMIT", "Balance would exceed supported limits.")
			return
		}
	}
	if err != nil {
		internal(c, err)
		return
	}
	_, err = tx.Exec(c, `INSERT INTO economy_transactions(player_id,project_id,environment_id,currency_id,operation,quantity,balance_after,reason,reference) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, playerID, p.ProjectID, p.EnvironmentID, currencyID, operation, in.Amount, after, in.Reason, in.Reference)
	if err != nil {
		internal(c, err)
		return
	}
	result := gin.H{"currency_key": in.CurrencyKey, "operation": operation, "amount": in.Amount, "balance": after}
	if err = s.idempotentSave(c, tx, actor, key, record, 200, result); err != nil {
		internal(c, err)
		return
	}
	if err = tx.Commit(c); err != nil {
		internal(c, err)
		return
	}
	success(c, 200, result)
}

// getTransactions returns economy history newest first. Pass meta.next_cursor from
// one page as ?cursor= to read the next; it is absent on the last page.
func (s *Server) getTransactions(c *gin.Context) {
	p := getPrincipal(c)
	limit := 50
	if x, e := strconv.Atoi(c.Query("limit")); e == nil && x > 0 && x <= 100 {
		limit = x
	}
	var beforeAt *time.Time
	var beforeID *string
	if cursor := c.Query("cursor"); cursor != "" {
		at, id, ok := decodeCursor(cursor)
		if !ok {
			failure(c, 400, "INVALID_CURSOR", "The pagination cursor is invalid.")
			return
		}
		beforeAt, beforeID = &at, &id
	}
	rows, err := s.db.Query(c, `SELECT t.id::text,t.operation,t.quantity,COALESCE(t.balance_after,0),COALESCE(d.currency_key,''),COALESCE(i.item_key,''),t.reason,t.reference,t.source,t.created_at
		FROM economy_transactions t LEFT JOIN currency_definitions d ON d.id=t.currency_id LEFT JOIN item_definitions i ON i.id=t.item_id
		WHERE t.player_id=$1 AND t.project_id=$2 AND t.environment_id=$3 AND ($4::timestamptz IS NULL OR (t.created_at,t.id) < ($4::timestamptz,$5::uuid))
		ORDER BY t.created_at DESC,t.id DESC LIMIT $6`, p.ID, p.ProjectID, p.EnvironmentID, beforeAt, beforeID, limit+1)
	if err != nil {
		internal(c, err)
		return
	}
	defer rows.Close()
	out := []gin.H{}
	var lastAt time.Time
	var lastID string
	more := false
	for rows.Next() {
		if len(out) == limit {
			more = true
			break
		}
		var id, op, cur, item, reason, ref, source string
		var amount, balance int64
		var at time.Time
		if err := rows.Scan(&id, &op, &amount, &balance, &cur, &item, &reason, &ref, &source, &at); err != nil {
			internal(c, err)
			return
		}
		lastAt, lastID = at, id
		out = append(out, gin.H{"id": id, "operation": op, "amount": amount, "balance": balance, "currency_key": cur, "item_key": item, "reason": reason, "reference": ref, "source": source, "created_at": at})
	}
	if err := rows.Err(); err != nil {
		internal(c, err)
		return
	}
	var meta gin.H
	if more {
		meta = gin.H{"next_cursor": encodeCursor(lastAt, lastID)}
	}
	successMeta(c, 200, out, meta)
}

func encodeCursor(at time.Time, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(at.UTC().Format(time.RFC3339Nano) + "|" + id))
}

func decodeCursor(cursor string) (time.Time, string, bool) {
	b, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return time.Time{}, "", false
	}
	at, id, found := strings.Cut(string(b), "|")
	t, err := time.Parse(time.RFC3339Nano, at)
	if !found || err != nil || !uuidPattern.MatchString(id) {
		return time.Time{}, "", false
	}
	return t, id, true
}

func (s *Server) grantItem(c *gin.Context) {
	var in itemMutation
	if !bind(c, &in) {
		return
	}
	p := getPrincipal(c)
	s.applyGrant(c, p, c.Param("playerId"), in)
}
func (s *Server) applyGrant(c *gin.Context, p principal, playerID string, in itemMutation) {
	if in.Quantity <= 0 || in.Quantity > 1e9 {
		failure(c, 422, "VALIDATION_ERROR", "Quantity must be positive and within limits.")
		return
	}
	if !s.playerInScope(c, p, playerID) {
		notFound(c)
		return
	}
	key := getIdempotencyKey(c)
	tx, err := s.db.Begin(c)
	if err != nil {
		internal(c, err)
		return
	}
	defer tx.Rollback(c)
	actor := p
	actor.ID = playerID
	record, replayStatus, ok := s.idempotentStart(c, tx, actor, key, c.FullPath(), in)
	if !ok {
		return
	}
	if replayStatus != 0 {
		_ = tx.Rollback(c)
		success(c, replayStatus, record)
		return
	}
	var itemID string
	var maxStack int64
	err = tx.QueryRow(c, `SELECT id::text,max_stack FROM item_definitions WHERE project_id=$1 AND environment_id=$2 AND item_key=$3 AND active`, p.ProjectID, p.EnvironmentID, in.ItemKey).Scan(&itemID, &maxStack)
	if err != nil {
		notFound(c)
		return
	}
	// The conflict update locks the stack and re-checks the limit, so concurrent grants
	// can never push a stack past max_stack.
	var after int64
	if in.Quantity <= maxStack {
		err = tx.QueryRow(c, `INSERT INTO inventory(player_id,project_id,environment_id,item_id,quantity) VALUES($1,$2,$3,$4,$5) ON CONFLICT(player_id,item_id) DO UPDATE SET quantity=inventory.quantity+EXCLUDED.quantity,updated_at=now() WHERE inventory.quantity+EXCLUDED.quantity<=$6 RETURNING quantity`, playerID, p.ProjectID, p.EnvironmentID, itemID, in.Quantity, maxStack).Scan(&after)
	} else {
		err = pgx.ErrNoRows
	}
	if errors.Is(err, pgx.ErrNoRows) {
		failure(c, 422, "STACK_LIMIT", "The requested quantity exceeds the item stack limit.")
		return
	}
	if err != nil {
		internal(c, err)
		return
	}
	_, err = tx.Exec(c, `INSERT INTO economy_transactions(player_id,project_id,environment_id,item_id,operation,quantity,reason,reference) VALUES($1,$2,$3,$4,'grant',$5,$6,$7)`, playerID, p.ProjectID, p.EnvironmentID, itemID, in.Quantity, in.Reason, in.Reference)
	if err != nil {
		internal(c, err)
		return
	}
	result := gin.H{"item_key": in.ItemKey, "operation": "grant", "quantity": in.Quantity, "owned": after}
	if err = s.idempotentSave(c, tx, actor, key, record, 200, result); err != nil {
		internal(c, err)
		return
	}
	if err = tx.Commit(c); err != nil {
		internal(c, err)
		return
	}
	success(c, 200, result)
}
func (s *Server) playerInScope(c *gin.Context, p principal, playerID string) bool {
	var exists bool
	err := s.db.QueryRow(c, `SELECT EXISTS(SELECT 1 FROM players WHERE id=$1 AND project_id=$2 AND environment_id=$3)`, playerID, p.ProjectID, p.EnvironmentID).Scan(&exists)
	return err == nil && exists
}

func (s *Server) getProgression(c *gin.Context) {
	p := getPrincipal(c)
	var xp int64
	var versionID string
	err := s.db.QueryRow(c, `SELECT total_xp,progression_version_id::text FROM player_progression WHERE player_id=$1 AND project_id=$2 AND environment_id=$3`, p.ID, p.ProjectID, p.EnvironmentID).Scan(&xp, &versionID)
	if errors.Is(err, pgx.ErrNoRows) {
		success(c, 200, gin.H{"total_xp": int64(0), "level": 1, "version": nil})
		return
	}
	if err != nil {
		internal(c, err)
		return
	}
	var raw []byte
	var version int
	err = s.db.QueryRow(c, `SELECT thresholds,version FROM progression_versions WHERE id=$1 AND project_id=$2 AND environment_id=$3`, versionID, p.ProjectID, p.EnvironmentID).Scan(&raw, &version)
	if err != nil {
		internal(c, err)
		return
	}
	var thresholds []int64
	_ = json.Unmarshal(raw, &thresholds)
	success(c, 200, gin.H{"total_xp": xp, "level": levelFor(thresholds, xp), "version": version, "thresholds": thresholds})
}

// levelFor returns the level for cumulative xp: level 1 at 0 XP, plus one for every
// threshold (levels 2+) reached.
func levelFor(thresholds []int64, xp int64) int {
	level := 1
	for _, threshold := range thresholds {
		if xp < threshold {
			break
		}
		level++
	}
	return level
}

func (s *Server) awardXP(c *gin.Context) {
	var in struct {
		Amount    int64  `json:"amount"`
		Reason    string `json:"reason"`
		Reference string `json:"reference"`
	}
	if !bind(c, &in) {
		return
	}
	if in.Amount <= 0 || in.Amount > 1e9 {
		failure(c, 422, "VALIDATION_ERROR", "XP amount must be positive and within limits.")
		return
	}
	p := getPrincipal(c)
	playerID := c.Param("playerId")
	// Check scope before the idempotency lookup: a malformed ID would otherwise fail
	// the UUID cast there and surface as a 500 instead of a 404.
	if !s.playerInScope(c, p, playerID) {
		notFound(c)
		return
	}
	key := getIdempotencyKey(c)
	tx, err := s.db.Begin(c)
	if err != nil {
		internal(c, err)
		return
	}
	defer tx.Rollback(c)
	actor := p
	actor.ID = playerID
	record, replayStatus, ok := s.idempotentStart(c, tx, actor, key, c.FullPath(), in)
	if !ok {
		return
	}
	if replayStatus != 0 {
		_ = tx.Rollback(c)
		success(c, replayStatus, record)
		return
	}
	var versionID string
	if err = tx.QueryRow(c, `SELECT 1 FROM players WHERE id=$1 AND project_id=$2 AND environment_id=$3 FOR UPDATE`, playerID, p.ProjectID, p.EnvironmentID).Scan(new(int)); err != nil {
		notFound(c)
		return
	}
	err = tx.QueryRow(c, `SELECT id::text FROM progression_versions WHERE project_id=$1 AND environment_id=$2 ORDER BY version DESC LIMIT 1`, p.ProjectID, p.EnvironmentID).Scan(&versionID)
	if err != nil {
		failure(c, 422, "PROGRESSION_NOT_PUBLISHED", "Publish progression thresholds before awarding XP.")
		return
	}
	// A new player pins to the latest version; an existing player keeps their pinned
	// version, which RETURNING reports (not the latest one).
	var xp int64
	var pinnedID string
	err = tx.QueryRow(c, `INSERT INTO player_progression(player_id,project_id,environment_id,progression_version_id,total_xp) VALUES($1,$2,$3,$4,$5) ON CONFLICT(player_id) DO UPDATE SET total_xp=player_progression.total_xp+EXCLUDED.total_xp,updated_at=now() RETURNING total_xp,progression_version_id::text`, playerID, p.ProjectID, p.EnvironmentID, versionID, in.Amount).Scan(&xp, &pinnedID)
	if err != nil {
		internal(c, err)
		return
	}
	var rawThresholds []byte
	var pinnedVersion int
	if err = tx.QueryRow(c, `SELECT thresholds,version FROM progression_versions WHERE id=$1`, pinnedID).Scan(&rawThresholds, &pinnedVersion); err != nil {
		internal(c, err)
		return
	}
	var thresholds []int64
	_ = json.Unmarshal(rawThresholds, &thresholds)
	_, err = tx.Exec(c, `INSERT INTO xp_transactions(player_id,project_id,environment_id,progression_version_id,amount,reason,reference,idempotency_key,request_hash) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, playerID, p.ProjectID, p.EnvironmentID, pinnedID, in.Amount, in.Reason, in.Reference, key, digest(fmt.Sprint(in.Amount, in.Reason, in.Reference)))
	if err != nil {
		internal(c, err)
		return
	}
	level, previous := levelFor(thresholds, xp), levelFor(thresholds, xp-in.Amount)
	result := gin.H{"total_xp": xp, "awarded": in.Amount, "level": level, "leveled_up": level > previous, "progression_version": pinnedVersion, "progression_version_id": pinnedID}
	if err = s.idempotentSave(c, tx, actor, key, record, 200, result); err != nil {
		internal(c, err)
		return
	}
	if err = tx.Commit(c); err != nil {
		internal(c, err)
		return
	}
	success(c, 200, result)
}

func (s *Server) submitScore(c *gin.Context) {
	var in struct {
		Score int64 `json:"score"`
	}
	if !bind(c, &in) {
		return
	}
	p := getPrincipal(c)
	key := getIdempotencyKey(c)
	tx, err := s.db.Begin(c)
	if err != nil {
		internal(c, err)
		return
	}
	defer tx.Rollback(c)
	record, replayStatus, ok := s.idempotentStart(c, tx, p, key, c.FullPath(), gin.H{"key": c.Param("key"), "score": in.Score})
	if !ok {
		return
	}
	if replayStatus != 0 {
		_ = tx.Rollback(c)
		success(c, replayStatus, record)
		return
	}
	var id, direction string
	var start, end *time.Time
	err = tx.QueryRow(c, `SELECT id::text,score_direction,season_start,season_end FROM leaderboard_definitions WHERE project_id=$1 AND environment_id=$2 AND key=$3 AND active`, p.ProjectID, p.EnvironmentID, c.Param("key")).Scan(&id, &direction, &start, &end)
	if err != nil {
		notFound(c)
		return
	}
	// The same timestamp decides the season check and is stored, so an accepted score
	// always falls inside the season it was accepted for (start inclusive, end exclusive).
	now := s.now().UTC()
	if start != nil && now.Before(*start) || end != nil && !now.Before(*end) {
		failure(c, 422, "LEADERBOARD_CLOSED", "The leaderboard is not accepting scores.")
		return
	}
	_, err = tx.Exec(c, `INSERT INTO score_submissions(leaderboard_id,player_id,project_id,environment_id,score,idempotency_key,request_hash,submitted_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, id, p.ID, p.ProjectID, p.EnvironmentID, in.Score, key, digest(fmt.Sprint(id, in.Score)), now)
	if err != nil {
		internal(c, err)
		return
	}
	result := gin.H{"leaderboard_key": c.Param("key"), "score": in.Score, "submitted_at": now}
	if err = s.idempotentSave(c, tx, p, key, record, 201, result); err != nil {
		internal(c, err)
		return
	}
	if err = tx.Commit(c); err != nil {
		internal(c, err)
		return
	}
	success(c, 201, result)
}
func (s *Server) getLeaderboard(c *gin.Context) {
	p := getScope(c)
	var boardID, direction string
	var start, end *time.Time
	err := s.db.QueryRow(c, `SELECT id::text,score_direction,season_start,season_end FROM leaderboard_definitions WHERE project_id=$1 AND environment_id=$2 AND key=$3 AND active`, p.ProjectID, p.EnvironmentID, c.Param("key")).Scan(&boardID, &direction, &start, &end)
	if err != nil {
		notFound(c)
		return
	}
	limit := 10
	if n, e := strconv.Atoi(c.DefaultQuery("limit", "10")); e == nil && n > 0 {
		limit = n
	}
	if limit > 100 {
		limit = 100
	}
	order := "DESC"
	if direction == "lower" {
		order = "ASC"
	}
	query := fmt.Sprintf(`WITH best AS (SELECT DISTINCT ON (ss.player_id) ss.player_id,ss.score,ss.submitted_at,pl.display_name FROM score_submissions ss JOIN players pl ON pl.id=ss.player_id WHERE ss.leaderboard_id=$1 AND ($2::timestamptz IS NULL OR ss.submitted_at >= $2) AND ($3::timestamptz IS NULL OR ss.submitted_at < $3) ORDER BY ss.player_id,ss.score %s,ss.submitted_at ASC,ss.id ASC), ranked AS (SELECT player_id,score,submitted_at,display_name,ROW_NUMBER() OVER(ORDER BY score %s,submitted_at ASC,player_id ASC) AS rank FROM best) SELECT player_id::text,score,submitted_at,display_name,rank FROM ranked ORDER BY rank LIMIT $4`, order, order)
	rows, err := s.db.Query(c, query, boardID, start, end, limit)
	if err != nil {
		internal(c, err)
		return
	}
	defer rows.Close()
	entries := []gin.H{}
	for rows.Next() {
		var id, name string
		var score int64
		var at time.Time
		var rank int64
		if err = rows.Scan(&id, &score, &at, &name, &rank); err != nil {
			internal(c, err)
			return
		}
		entries = append(entries, gin.H{"player_id": id, "display_name": name, "score": score, "rank": rank, "submitted_at": at})
	}
	if err = rows.Err(); err != nil {
		internal(c, err)
		return
	}
	rows.Close()
	result := gin.H{"key": c.Param("key"), "entries": entries}
	if player, ok := c.Get("principal"); ok {
		pl := player.(principal)
		var rank, score int64
		err = s.db.QueryRow(c, fmt.Sprintf(`WITH best AS (SELECT DISTINCT ON (player_id) player_id,score,submitted_at FROM score_submissions WHERE leaderboard_id=$1 AND ($2::timestamptz IS NULL OR submitted_at >= $2) AND ($3::timestamptz IS NULL OR submitted_at < $3) ORDER BY player_id,score %s,submitted_at ASC,id ASC), ranked AS (SELECT player_id,score,ROW_NUMBER() OVER(ORDER BY score %s,submitted_at ASC,player_id ASC) rank FROM best) SELECT rank,score FROM ranked WHERE player_id=$4`, order, order), boardID, start, end, pl.ID).Scan(&rank, &score)
		if err == nil {
			result["player_rank"] = rank
			result["player_score"] = score
		} else if !errors.Is(err, pgx.ErrNoRows) {
			internal(c, err)
			return
		}
	}
	success(c, 200, result)
}
func getScope(c *gin.Context) principal { v, _ := c.Get("scope"); return v.(principal) }

func randomUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return randomID()
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
