package httpapi

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"realmstack-backend/internal/achievements"
	"realmstack-backend/internal/auth"
	"realmstack-backend/internal/catalog"
	"realmstack-backend/internal/economy"
	"realmstack-backend/internal/inventory"
	"realmstack-backend/internal/leaderboards"
	"realmstack-backend/internal/platform"
	"realmstack-backend/internal/players"
	"realmstack-backend/internal/progression"
	"realmstack-backend/internal/projects"
	"realmstack-backend/internal/quests"
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
	inventoryH := inventory.Handler{Env: env}
	economyH := economy.Handler{Env: env}
	progressionH := progression.Handler{Env: env}
	leaderboardsH := leaderboards.Handler{Env: env}
	statsH := stats.Handler{Env: env}
	achievementsH := achievements.Handler{Env: env}
	questsH := quests.Handler{Env: env}

	r := s.router
	r.GET("/healthz", func(c *gin.Context) { platform.Success(c, http.StatusOK, gin.H{"status": "ok"}) })
	v := r.Group("/v1")

	// Dashboard authentication (public, rate limited).
	v.POST("/auth/register", s.rateLimit(), authH.Register)
	v.POST("/auth/login", s.rateLimit(), authH.Login)

	// Dashboard: projects owned by the signed-in user.
	user := v.Group("", s.userAuth())
	user.GET("/projects", projectsH.List)
	user.POST("/projects", projectsH.Create)
	user.GET("/projects/:projectId", projectsH.Get)
	user.POST("/credentials/:id/revoke", projectsH.RevokeCredential)
	project := user.Group("/projects/:projectId", projectsH.RequireOwnedProject())
	project.PATCH("", projectsH.Update)
	project.GET("/environments", projectsH.ListEnvironments)
	project.POST("/environments", projectsH.CreateEnvironment)

	// Dashboard: one owned environment.
	environment := user.Group("/projects/:projectId/environments/:envId", projectsH.RequireOwnedEnvironment())
	environment.GET("/credentials", projectsH.ListCredentials)
	environment.POST("/credentials", projectsH.IssueCredential)
	environment.GET("/configurations/draft", catalogH.GetDraft)
	environment.PUT("/configurations/draft", catalogH.PutDraft)
	environment.POST("/configurations/validate", catalogH.ValidateDraft)
	environment.POST("/configurations/publish", catalogH.Publish)
	environment.GET("/configurations/versions", catalogH.ListVersions)
	environment.GET("/configurations/published", catalogH.GetPublished)
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
	me := v.Group("/players/me", s.playerAuth())
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

	// Trusted game server (server credential; scope comes from the credential).
	server := v.Group("/server/players/:playerId", s.serverAuth())
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

	// Leaderboards.
	v.POST("/leaderboards/:key/scores", s.playerAuth(), leaderboardsH.Submit)
	v.GET("/leaderboards/:key", s.publicScope(), leaderboardsH.Get)
}
