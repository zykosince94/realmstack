package httpapi

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"realmstack-backend/internal/achievements"
	"realmstack-backend/internal/auth"
	"realmstack-backend/internal/bans"
	"realmstack-backend/internal/batch"
	"realmstack-backend/internal/catalog"
	"realmstack-backend/internal/economy"
	"realmstack-backend/internal/inventory"
	"realmstack-backend/internal/leaderboards"
	"realmstack-backend/internal/platform"
	"realmstack-backend/internal/playeradmin"
	"realmstack-backend/internal/players"
	"realmstack-backend/internal/progression"
	"realmstack-backend/internal/projects"
	"realmstack-backend/internal/quests"
	"realmstack-backend/internal/saves"
	"realmstack-backend/internal/stats"
)

// routes is the whole API surface. Every route is also documented in
// api/openapi.yaml; TestOpenAPICoversEveryRoute fails if the two drift apart.
func (s *Server) routes() {
	env := s.Env
	authH := auth.Handler{Env: env}
	projectsH := projects.Handler{Env: env}
	catalogH := catalog.Handler{Env: env}
	playersH := players.Handler{Env: env}
	playerAdminH := playeradmin.Handler{Env: env}
	inventoryH := inventory.Handler{Env: env}
	economyH := economy.Handler{Env: env}
	progressionH := progression.Handler{Env: env}
	leaderboardsH := leaderboards.Handler{Env: env}
	statsH := stats.Handler{Env: env}
	achievementsH := achievements.Handler{Env: env}
	questsH := quests.Handler{Env: env}
	batchH := batch.Handler{Env: env, Quotas: s.Quotas}
	savesH := saves.Handler{Env: env}
	bansH := bans.Handler{Env: env, Cache: s.Bans}

	r := s.router
	r.GET("/healthz", func(c *gin.Context) { platform.Success(c, http.StatusOK, gin.H{"status": "ok"}) })
	v := r.Group("/v1")

	// Dashboard authentication (public, rate limited).
	v.POST("/auth/register", s.rateLimit(), authH.Register)
	v.POST("/auth/login", s.rateLimit(), authH.Login)
	// Session resume and sign-out use the httpOnly refresh cookie, not a bearer token.
	v.POST("/auth/refresh", s.rateLimit(), authH.Refresh)
	v.POST("/auth/logout", authH.Logout)

	// Dashboard: projects owned by the signed-in user.
	user := v.Group("", s.userAuth())
	user.GET("/projects", projectsH.List)
	user.POST("/projects", projectsH.Create)
	user.GET("/projects/:projectId", projectsH.Get)
	user.POST("/credentials/:id/revoke", projectsH.RevokeCredential)
	// AI assistant settings belong to the dashboard user (spec 4: ai_provider_settings).
	aiUser := user.Group("/ai", s.AI.RequireEnabled())
	aiUser.GET("/settings", s.AI.GetSettings)
	aiUser.PUT("/settings", s.AI.PutSettings)
	aiUser.DELETE("/settings", s.AI.DeleteSettings)
	aiUser.POST("/settings/test", s.AI.TestSettings)
	project := user.Group("/projects/:projectId", projectsH.RequireOwnedProject())
	project.PATCH("", projectsH.Update)
	project.GET("/environments", projectsH.ListEnvironments)
	project.POST("/environments", projectsH.CreateEnvironment)

	// Dashboard: one owned environment.
	environment := user.Group("/projects/:projectId/environments/:envId", projectsH.RequireOwnedEnvironment())

	// Players of an owned environment, read-only. The detail routes reuse the game's own
	// read handlers; Target narrows the request to one player of this environment.
	environment.GET("/players", playerAdminH.List)
	adminPlayer := environment.Group("/players/:playerId", playerAdminH.Target())
	adminPlayer.GET("", playerAdminH.Profile)
	adminPlayer.GET("/inventory", inventoryH.List)
	adminPlayer.GET("/currency", economyH.Balances)
	adminPlayer.GET("/transactions", economyH.Transactions)
	adminPlayer.GET("/progression", progressionH.Get)
	adminPlayer.GET("/stats", statsH.ForPlayer)
	adminPlayer.GET("/achievements", achievementsH.ForPlayer)
	adminPlayer.GET("/quests", questsH.ForPlayer)
	// Player management: an admin performs, on behalf of one player, the actions a game
	// client or server would. They are the same handlers and transaction functions (so
	// the same rules, ledgers and goal evaluation), reached with a dashboard token and
	// an Idempotency-Key. Claims use the server variants, so an admin can claim a goal
	// whatever its claim policy.
	adminPlayer.POST("/items/grant", inventoryH.GrantHandler)
	adminPlayer.POST("/inventory/consume", inventoryH.Consume)
	adminPlayer.POST("/inventory/equip", inventoryH.Equip)
	adminPlayer.POST("/inventory/unequip", inventoryH.Unequip)
	adminPlayer.POST("/currency/credit", economyH.CreditHandler)
	adminPlayer.POST("/currency/spend", economyH.SpendHandler)
	adminPlayer.POST("/xp/award", progressionH.AwardHandler)
	adminPlayer.POST("/stats/:statKey/increment", statsH.Increment)
	adminPlayer.POST("/flags/:flagKey/set", statsH.SetFlag)
	adminPlayer.POST("/achievements/:key/claim", achievementsH.ClaimForPlayer)
	adminPlayer.POST("/quests/:key/accept", questsH.AcceptForPlayer)
	adminPlayer.POST("/quests/:key/abandon", questsH.AbandonMine)
	adminPlayer.POST("/quests/:key/claim", questsH.ClaimForPlayer)
	adminPlayer.POST("/leaderboards/:key/scores", leaderboardsH.Submit)
	adminPlayer.GET("/saves", savesH.List)
	adminPlayer.GET("/saves/:slot", savesH.Get)
	adminPlayer.PUT("/saves/:slot", savesH.Put)
	adminPlayer.DELETE("/saves/:slot", savesH.Delete)
	// Bans are dashboard-only (no game route bans a player). They are enforced at login,
	// refresh and on every player-token request (playerAuth).
	adminPlayer.GET("/bans", bansH.History)
	adminPlayer.POST("/ban", bansH.Ban)
	adminPlayer.POST("/ban/lift", bansH.Lift)
	environment.GET("/credentials", projectsH.ListCredentials)
	environment.POST("/credentials", projectsH.IssueCredential)
	environment.GET("/configurations/draft", catalogH.GetDraft)
	environment.PUT("/configurations/draft", catalogH.PutDraft)
	environment.POST("/configurations/validate", catalogH.ValidateDraft)
	environment.POST("/configurations/publish", catalogH.Publish)
	environment.GET("/configurations/versions", catalogH.ListVersions)
	environment.GET("/configurations/published", catalogH.GetPublished)
	environment.POST("/configurations/versions/:version/rollback", catalogH.Rollback)
	// AI proposals for this environment. Applying one edits the draft only.
	aiEnv := environment.Group("/ai", s.AI.RequireEnabled())
	aiEnv.GET("/generations", s.AI.ListGenerations)
	aiEnv.POST("/generations", s.AI.CreateGeneration)
	aiEnv.GET("/generations/:id", s.AI.GetGeneration)
	aiEnv.POST("/generations/:id/apply", s.AI.Apply)
	aiEnv.POST("/generations/:id/discard", s.AI.Discard)
	environment.GET("/progression", catalogH.GetProgression)
	environment.PUT("/progression", catalogH.PutProgression)
	environment.GET("/achievements/stats", achievementsH.Stats)
	environment.GET("/quests/stats", questsH.Stats)
	for _, name := range catalog.DraftCollections {
		kind := catalog.DefinitionKinds[name]
		environment.GET("/"+name, catalogH.ListDefinitions(kind))
		environment.POST("/"+name, catalogH.CreateDefinition(kind))
		environment.GET("/"+name+"/:key", catalogH.GetDefinition(kind))
		environment.PUT("/"+name+"/:key", catalogH.ReplaceDefinition(kind))
		environment.DELETE("/"+name+"/:key", catalogH.DeleteDefinition(kind))
	}

	// Player identity (public environment identifier, rate limited).
	v.POST("/players/register", s.rateLimit(), playersH.Register)
	v.POST("/players/login", s.rateLimit(), playersH.Login)
	v.POST("/players/refresh", playersH.Refresh)

	// Player self-service (player token; scope comes from the token).
	me := v.Group("/players/me", s.playerAuth(), s.quota())
	me.GET("", playersH.Me)
	me.PATCH("", playersH.PatchMe)
	me.GET("/inventory", inventoryH.List)
	me.GET("/inventory/:itemKey", inventoryH.Get)
	me.POST("/inventory/consume", inventoryH.Consume)
	me.POST("/inventory/equip", inventoryH.Equip)
	me.POST("/inventory/unequip", inventoryH.Unequip)
	me.GET("/currency", economyH.Balances)
	me.POST("/currency/spend", economyH.SpendHandler)
	me.GET("/transactions", economyH.Transactions)
	me.GET("/progression", progressionH.Get)
	me.GET("/stats", statsH.Mine)
	me.GET("/achievements", achievementsH.Mine)
	me.POST("/achievements/:key/claim", achievementsH.ClaimMine)
	me.GET("/quests", questsH.Mine)
	me.POST("/quests/:key/accept", questsH.AcceptMine)
	me.POST("/quests/:key/abandon", questsH.AbandonMine)
	me.POST("/quests/:key/claim", questsH.ClaimMine)
	me.GET("/saves", savesH.List)
	me.GET("/saves/:slot", savesH.Get)
	me.PUT("/saves/:slot", savesH.Put)
	me.DELETE("/saves/:slot", savesH.Delete)

	// Trusted game server (server credential; scope comes from the credential).
	// Many trusted writes in one request; each entry is idempotent on its own.
	v.POST("/server/batch", s.serverAuth(), s.quota(), batchH.Write)
	server := v.Group("/server/players/:playerId", s.serverAuth(), s.quota())
	server.POST("/items/grant", inventoryH.GrantHandler)
	server.POST("/currency/credit", economyH.CreditHandler)
	server.POST("/xp/award", progressionH.AwardHandler)
	server.GET("/stats", statsH.ForPlayer)
	server.POST("/stats/:statKey/increment", statsH.Increment)
	server.POST("/flags/:flagKey/set", statsH.SetFlag)
	server.GET("/achievements", achievementsH.ForPlayer)
	server.POST("/achievements/:key/claim", achievementsH.ClaimForPlayer)
	server.GET("/quests", questsH.ForPlayer)
	server.POST("/quests/:key/accept", questsH.AcceptForPlayer)
	server.POST("/quests/:key/claim", questsH.ClaimForPlayer)
	server.GET("/saves", savesH.List)
	server.GET("/saves/:slot", savesH.Get)
	server.PUT("/saves/:slot", savesH.Put)
	server.DELETE("/saves/:slot", savesH.Delete)

	// Leaderboards.
	v.POST("/leaderboards/:key/scores", s.playerAuth(), s.quota(), leaderboardsH.Submit)
	v.GET("/leaderboards/:key", s.publicScope(), s.quota(), leaderboardsH.Get)
}
