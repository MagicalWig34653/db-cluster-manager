---
title: Manager HTTPS and ACME
---

# Manager HTTPS and ACME

[Guide index](index.md) · [Configuration reference](12-configuration-reference.md)

The manager can terminate HTTPS itself using ACME HTTP-01 or TLS-ALPN-01 [Fakes]. The default CA is Let's Encrypt **staging**, whose certificates browsers do not trust. No public CA or real infrastructure was contacted in this implementation's tests. This changes only the manager API/UI listener; agent enrollment, peer mTLS and database TLS have separate configuration.

Add the following section inside your existing `manager` configuration (keep its `dataDir`, `bind`, authentication and other settings):

```json
{
  "tls": {
    "acme": {
      "directoryUrl": "https://acme-staging-v02.api.letsencrypt.org/directory",
      "email": "ops@example.com",
      "domains": ["manager.example.com"],
      "challenge": "tls-alpn-01"
    }
  }
}
```

Start with `manager -config manager.json -listen 0.0.0.0:8443`. Public DNS for every configured domain must reach these managers. For TLS-ALPN-01, public TCP **443** must reach this TLS listener, preserving SNI and `acme-tls/1`. For HTTP-01, set `challenge` to `http-01` and optionally `httpListen` (default `:80`); public TCP **80** must reach that separate listener. It exposes only active challenge tokens, with no API, UI or redirect. Configure network forwarding and port permissions yourself. The shipped non-root, capability-free container cannot be assumed to bind privileged ports; use unprivileged local ports and external forwarding. The shipped deployment files still use loopback HTTP behind an operator's proxy and are not automatically rewritten for ACME.

A TLS-terminating proxy that consumes ALPN will prevent TLS-ALPN-01 validation. Use TCP passthrough, or HTTP-01 with the challenge path routed to the manager. All manager replicas must use identical ACME settings, the same replicated metastore and accessible data keys (`manager.keyring` or the same manually distributed key). Certificates, account keys and short-lived challenge records are AES-GCM sealed with collection/key binding before entering Raft logs and snapshots. Followers can serve certificates and challenges from their local replicated view; lagging or unavailable replicas can temporarily fail a challenge. The API still requires routing to the current leader.

Only the leader requests or renews certificates. It checks on election and hourly, renewing within 30 days of expiry; errors retry after five minutes. A leadership loss cancels the current order and fences storage writes/cleanup to that term. A new leader reuses the persisted account key and can start a new order. A failed renewal keeps the previous certificate; without an initial valid certificate, HTTPS handshakes fail until issuance succeeds. Check the manager log for `ACME issuance/renewal failed`. There is no HTTP fallback and no issuance triggered by client handshakes.

Enabling ACME accepts the configured CA's terms of service. Supply a plain email address and lowercase ASCII/punycode DNS names; IP identifiers and wildcards are refused. DNS-01 remains a separate, unwired certificate interface. External account binding, revocation and account-key rollover are not implemented. Configuration changes require restart; changing CA, email, domains or challenge selects separate stored state. Existing records remain sealed in the metastore.

After staging validation, explicitly change `directoryUrl` to `https://acme-v02.api.letsencrypt.org/directory` for publicly trusted certificates. This requests new production certificates; staging certificates are never promoted. `-healthcheck -config manager.json -listen 0.0.0.0:8443` uses verified HTTPS over loopback, with the first configured domain as SNI. For staging or private CAs, install the CA in the trust store (for example `SSL_CERT_FILE`); verification is never skipped. A container healthcheck needs a startup grace period long enough for first issuance.

For an imported certificate, configure `manager.tls.certFile` and `keyFile` instead of `acme`; both PEM files are required and loaded at startup (restart to replace them). Plain HTTP is permitted by default only on numeric loopback addresses. An exposed HTTP listener needs the explicit `manager.tls.allowPlainHttp: true`, intended for an operator-managed TLS proxy on a protected network. A hostname bind, including `localhost`, is refused without explicit configuration; use `127.0.0.1` or `::1`. Do not enable this flag for a direct public listener.

Verification uses a fake RFC 8555 CA with `httptest` response recorders, both challenge proofs, renewal/failure/cancellation, a `net.Pipe` TLS handshake and a three-node in-memory Raft failover/storage test. The additional socket-backed `httptest.Server` variants skip if the sandbox refuses listeners; that skip provides no network-listener evidence. See [current results](../../STATUS.md#manager-listener-acmetls-2026-10-24-fakes-only). Real DNS, public CA validation, browser trust and an HA load balancer remain unverified.
