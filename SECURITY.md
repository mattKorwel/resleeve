# Security policy

## Supported versions

Resleeve is in pre-release (`v0.x.y`). No version is yet production-ready or eligible for security backports. Coordinated disclosure begins at `v1.0.0`.

## Reporting a vulnerability

Email `security@resleeve.dev` (once the domain is registered). Until then, use GitHub's "Report a vulnerability" private reporting feature on the [repo's security tab](https://github.com/mattkorwel/resleeve/security/advisories).

Do **not** file a public issue for vulnerabilities.

We aim to respond within 5 business days during pre-release; faster after `v1.0.0`.

## Scope

Vulnerabilities of interest:

- Authentication bypass (token forgery, replay, etc.)
- Privilege escalation across users or scopes
- Data leakage (sessions, plans, learnings visible to unauthorized viewers)
- Cryptographic flaws (KEK derivation, share encryption, signature verification)
- Webhook signature bypass

Out of scope (pre-release):

- Self-XSS
- Denial of service requiring authenticated user
- Issues in third-party dependencies (please file upstream)
