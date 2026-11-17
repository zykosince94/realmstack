// k6 load test for the spec 6.6 target profile: a large tenant with ~500k registered
// players (the spec says ~1M; reduced by decision, 2026-11-17), ~100k concurrent,
// ~5,000 trusted writes per second, and a login surge of ~100k players in 10 minutes
// (~167 logins/s).
//
// "100k concurrent players" is modelled as request rates, not 100k virtual users: an
// online player's client polls now and then, so constant-arrival-rate scenarios drive
// the rates those players produce. SCALE multiplies every rate (SCALE=0.2 for a quick
// run); DURATION sets the length of the main phase.
//
// Pass criteria (spec 10, Phase 8 exit): p95 under 150 ms for writes, under 50 ms for
// leaderboard top-100 and player rank.
import http from "k6/http";
import { check } from "k6";
import { SharedArray } from "k6/data";

// Parse the seed file once into read-only shared memory. A plain JSON.parse(open(...))
// here runs in every VU's init context: with hundreds of VUs that re-parsed the 2 MB
// file hundreds of times and exhausted the machine's memory before a request was sent.
const tenant = new SharedArray("tenant", () => {
  const t = JSON.parse(open("./data/tenant.json"));
  return [{ base_urls: t.base_urls, public_environment_id: t.public_environment_id, credential: t.credential, password: t.password }];
})[0];
const players = new SharedArray("players", () => JSON.parse(open("./data/tenant.json")).players);
const BASES = (__ENV.BASE_URLS || tenant.base_urls.join(",")).split(",");
const SCALE = parseFloat(__ENV.SCALE || "1");
const DURATION = __ENV.DURATION || "5m";
const LOGIN_DURATION = __ENV.LOGIN_DURATION || "2m";
const TOKENS = parseInt(__ENV.TOKENS || "400", 10);
const rate = (n) => Math.max(1, Math.round(n * SCALE));

function scenario(fn, perSecond, duration, vus) {
  return { executor: "constant-arrival-rate", exec: fn, rate: rate(perSecond), timeUnit: "1s", duration, preAllocatedVUs: Math.ceil(vus * Math.max(SCALE, 0.25)), maxVUs: Math.ceil(vus * 2 * Math.max(SCALE, 0.25)) };
}

const allScenarios = {
  // ~5,000 trusted writes/s: 4,500 single writes + 10 batches of 50 entries.
  server_writes: scenario("serverWrite", 4500, DURATION, 400),
  batch_writes: scenario("batchWrite", 10, DURATION, 20),
  // Players submitting scores, reading leaderboards and polling their state.
  score_submits: scenario("submitScore", 150, DURATION, 30),
  leaderboard_top: scenario("leaderboardTop", 400, DURATION, 60),
  leaderboard_rank: scenario("leaderboardRank", 400, DURATION, 60),
  player_reads: scenario("playerRead", 2000, DURATION, 200),
  // Login surge: 100k logins in 10 minutes is ~167/s (bcrypt cost 10).
  login_surge: scenario("login", 167, LOGIN_DURATION, 200),
};

// SCENARIOS=server_writes,login_surge runs a subset (default: all), e.g. to measure
// the steady traffic and the login surge separately.
const only = (__ENV.SCENARIOS || "").split(",").filter(Boolean);
const scenarios = only.length ? Object.fromEntries(only.map((n) => [n, allScenarios[n]])) : allScenarios;

export const options = {
  setupTimeout: "5m",
  scenarios,
  thresholds: {
    "http_req_duration{kind:write}": ["p(95)<150"],
    "http_req_duration{kind:leaderboard}": ["p(95)<50"],
    "http_req_duration{kind:rank}": ["p(95)<50"],
    http_req_failed: ["rate<0.01"],
    checks: ["rate>0.99"],
  },
  summaryTrendStats: ["avg", "med", "p(90)", "p(95)", "p(99)", "max"],
};

let counter = 0;
const base = () => BASES[(counter++) % BASES.length];
const pick = (arr) => arr[Math.floor(Math.random() * arr.length)];
const key = () => `${__VU}-${__ITER}-${Date.now()}-${Math.random().toString(36).slice(2, 10)}`;
const serverHeaders = (extra) => Object.assign({ "Content-Type": "application/json", "X-Server-Credential": tenant.credential }, extra || {});

// Log some players in once; read scenarios reuse their tokens.
export function setup() {
  const reqs = [];
  for (let i = 0; i < TOKENS; i++) {
    const p = players[i % players.length];
    reqs.push(["POST", `${BASES[0]}/v1/players/login`, JSON.stringify({ public_environment_id: tenant.public_environment_id, email: p.email, password: tenant.password }),
      { headers: { "Content-Type": "application/json" }, tags: { kind: "setup" } }]);
  }
  const tokens = [];
  for (let i = 0; i < reqs.length; i += 50) {
    for (const res of http.batch(reqs.slice(i, i + 50))) {
      if (res.status === 200) tokens.push(res.json("data.access_token"));
    }
  }
  if (tokens.length === 0) throw new Error("setup could not log any player in");
  return { tokens };
}

const writes = [
  (p) => ["/items/grant", { item_key: "potion", quantity: 1, reason: "drop" }],
  (p) => ["/currency/credit", { currency_key: "gold", amount: 10, reason: "match" }],
  (p) => ["/xp/award", { amount: 5, reason: "match" }],
  (p) => ["/stats/kills/increment", { amount: 1, reason: "combat" }],
];

export function serverWrite() {
  const p = pick(players);
  const [path, body] = pick(writes)(p);
  const res = http.post(`${base()}/v1/server/players/${p.id}${path}`, JSON.stringify(body), { headers: serverHeaders({ "Idempotency-Key": key() }), tags: { kind: "write" } });
  check(res, { "write 200": (r) => r.status === 200 });
}

export function batchWrite() {
  const entries = [];
  for (let i = 0; i < 50; i++) {
    const p = pick(players);
    entries.push(i % 2 === 0
      ? { type: "credit", idempotency_key: key() + i, player_id: p.id, currency_key: "gold", amount: 5 }
      : { type: "stat", idempotency_key: key() + i, player_id: p.id, stat_key: "kills", amount: 1 });
  }
  const res = http.post(`${base()}/v1/server/batch`, JSON.stringify({ entries }), { headers: serverHeaders(), tags: { kind: "batch" } });
  check(res, { "batch applied": (r) => r.status === 200 && r.json("data.failed") === 0 });
}

export function submitScore(data) {
  const res = http.post(`${base()}/v1/leaderboards/arena/scores`, JSON.stringify({ score: Math.floor(Math.random() * 100000) }),
    { headers: { "Content-Type": "application/json", Authorization: `Bearer ${pick(data.tokens)}`, "Idempotency-Key": key() }, tags: { kind: "score" } });
  check(res, { "score 201": (r) => r.status === 201 });
}

export function leaderboardTop() {
  const res = http.get(`${base()}/v1/leaderboards/arena?environment_id=${tenant.public_environment_id}&limit=100`, { tags: { kind: "leaderboard" } });
  check(res, { "top 100": (r) => r.status === 200 && r.json("data.entries").length === 100 });
}

export function leaderboardRank(data) {
  // limit=1 isolates the rank lookup (one indexed count) from the top-N scan.
  const res = http.get(`${base()}/v1/leaderboards/arena?limit=1`, { headers: { Authorization: `Bearer ${pick(data.tokens)}` }, tags: { kind: "rank" } });
  check(res, { "rank": (r) => r.status === 200 && r.json("data.player_rank") > 0 });
}

const reads = ["/v1/players/me/inventory", "/v1/players/me/currency", "/v1/players/me/progression", "/v1/players/me/achievements", "/v1/players/me/quests"];

export function playerRead(data) {
  const res = http.get(`${base()}${pick(reads)}`, { headers: { Authorization: `Bearer ${pick(data.tokens)}` }, tags: { kind: "read" } });
  check(res, { "read 200": (r) => r.status === 200 });
}

export function login() {
  const p = pick(players);
  const res = http.post(`${base()}/v1/players/login`, JSON.stringify({ public_environment_id: tenant.public_environment_id, email: p.email, password: tenant.password }),
    { headers: { "Content-Type": "application/json" }, tags: { kind: "login" } });
  check(res, { "login 200": (r) => r.status === 200 });
}
