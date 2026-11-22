# Security policy

## Supported versions

Security fixes are made to the latest 1.x release. Upgrade to it before reporting an issue
you found on an older version.

## Reporting a vulnerability

Please report vulnerabilities privately. Do not open a public issue, pull request or
discussion that contains exploit details.

Use GitHub's private vulnerability reporting: open this repository's **Security** tab and
choose **Report a vulnerability**. Include:

- the affected route or component and the version or commit you tested;
- the steps or request that reproduce it (a minimal case is best);
- the impact you believe it has, for example cross-tenant access or forged tokens;
- the `X-Request-ID` of a failing request, if you have one.

Reports are handled on a best-effort basis. Please allow reasonable time to investigate and
release a fix before you disclose publicly; 90 days is a common default. Tell us if you want
to be credited.

## Scope

In scope: tenant isolation (project and environment scope), authentication and token
handling, server credentials, idempotency and ledger integrity, the AI base-URL guard
(SSRF), secret handling and injection.

Out of scope:

- The local demo credentials and demo seed. They exist only when `APP_ENV=local` and
  `DEMO_MODE=true`, and are documented as unsafe for anything else.
- Volumetric denial of service. Rate limits and quotas are configurable; capacity is your
  deployment's responsibility.
- Missing features listed under **Known limits** in the documentation, such as MFA or an
  audit log.

## Hardening a deployment

The documentation's **Production checklist** and **Self-hosting** pages cover secrets, TLS,
trusted proxies, database roles and backups. Set a random `JWT_SECRET`, run with
`APP_ENV=production` and `DEMO_MODE=false`, and keep the API on a private network behind a
TLS proxy.
