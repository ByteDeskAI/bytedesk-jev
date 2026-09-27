# Jev for ByteDesk Gateway

An independently installable Go process plugin, based on the Gateway v2 template.
It exposes Typesafe's Jev decision primitives to other enabled plugins through the
public host-owned AI decision API. Jev is not a text-generation or coding model.

## Install and configure

The intended delivery path is the ByteDesk Store. The first artifact targets
Linux amd64. Source preparation or a local build is not a published Store release.

1. Install and enable **Jev** on a compatible Gateway.
2. In **Gateway Settings → Jev**, enter the Typesafe API key. The host stores the
   secret; it is never returned to this plugin or its playground.
3. Keep the default pinned model `jev-1.13.0`, or explicitly enable model aliases
   before selecting `jev-latest` or `jev-preview`.
4. Optionally set requests-per-minute and daily input-token limits. Each limit
   applies separately to each consumer plugin, not an aggregate Jev total. Unset
   means no additional local limit. The host owns accounting and admission; unknown
   usage is not zero, and in-flight work can cause bounded budget overshoot.

The plugin requires `host.workload-auth.v1` and the complete `ai.decision.v1`
host feature. Older or partially implemented hosts must refuse activation.
Coding playground operations also need the host's complete `coding.sessions.v1`
implementation; the plugin does not emulate it when unavailable.

## Consumer contract

Consumers declare exact `aidecision` command permissions in their SDK manifest
and call the public host facade with provider ID `jev`. The host resolves the
enabled provider, authorizes the consumer, creates an invocation scope, and owns
the consumer-visible job. Consumers never call `svc.jev.*` directly.

Supported questions can be mixed in one batch:

- **Choice:** 1–255 stable option IDs with structured descriptions or null.
- **Score:** 2–10 ordered rubric levels; scores and legend indexes are zero-based.
- **Noul:** a probability between zero and one, with optional true/false criteria.

State, instructions and criteria accept SDK text or structured JSON, inline or
through scoped host payload handles. Decimal values remain exact strings in the
shared result DTOs. The adapter rejects duplicate JSON keys, mismatched question
IDs/types, invalid probability distributions, incomplete usage, and a response
from a different pinned model. Explicit aliases retain both requested and actual
resolved model IDs.

The host transport permits up to 8 MiB assembled payloads in 24 KiB chunks; this
is not the model context window. Typesafe documents 64k tokens across a request
and 32k for state plus the longest question. Without Typesafe's tokenizer this
adapter applies conservative UTF-8/JSON byte admission bounds (62,976 total,
31,488 state plus one question). These are not billed-token estimates. Typesafe
remains authoritative and may reject an input with HTTP 422.

## Ownership and lifecycle

Gateway owns credentials, HTTPS destinations, request retries, budgets, payload
storage and all coding processes. Jev can request only named host operations
`evaluate` and `models`; it contains no outbound HTTP client or executable path.
Every nested payload and egress operation forwards the host-minted invocation
handle. Native host-only provider ingress is a required broker guarantee; a
`bd-caller` header is not treated as proof of identity.

Provider handlers return promptly. Evaluation jobs have a 30-second total
deadline; routing jobs have 10 seconds. The host owns at most two transient
retries within that deadline, including Retry-After; the adapter never adds a
second retry loop. Cancellation requests cancel host egress and revoke temporary
payloads and credential handles. Up to 256 provider jobs are retained for five
minutes; these are generation-local work records, not durable coding sessions.
Model discovery returns an immediate cached snapshot and refreshes asynchronously.

## Playground

The SDK-mounted panel has a mixed primitive editor, actual host route preview,
coding-provider discovery, and controls to start/attach a durable coding task,
send followups, stop a prompt, answer offered approvals, complete/end a task and
open that same task in the terminal dock. Enter a Gateway project ID and committed
checkout reference; the host resolves them and creates the isolated worktree.
Preview creates no session, process or worktree, but may incur Jev usage.

Coding execution requires the Gateway administrator's supervised-process consent
and the exact coding command grants. The manifest declares this capability; it
does not grant itself consent or launch processes directly. Reading and ending an
existing task remain available only with the host's ownership and command checks.

Navigating away only detaches the playground view. It never ends a task. Explicit
**End session**, or closing its docked terminal tab, ends the host session.
The playground renders host-reported availability and errors; it never fabricates
a selected route or successful coding run. Its latest 300 displayed events are a
bounded view of the host's durable history; long individual events are marked as
display-truncated, not silently treated as complete.

## Build and verify

Dependencies must be released SDK versions; `replace` and `go.work` are not used.

```sh
go test ./... -count=1
go test ./... -race -count=1
go vet ./...
node --test tests/*.test.mjs
go run ./cmd/manifest -out plugin.json
```

Concrete provider descriptors are generated from the pinned common SDK:

```sh
go run github.com/ByteDeskAI/bytedesk-sdk-dependencies/v2/cmd/contractgen -emit=go -package=aidecision -provider-id=jev -go-package=decision -out contracts/decision/generated.go
go run github.com/ByteDeskAI/bytedesk-sdk-dependencies/v2/cmd/contractgen -emit=go -package=hostsettings -provider-id=jev -go-package=settings -out contracts/settings/generated.go
```

`scripts/ci/plugin-build-v1.sh /absolute/empty/staging-directory` implements
TemplatePluginBuildV1 and stages `jev` plus generated `plugin.json`. Panel assets
are embedded in the binary. The trusted shared TeamCity lane, not this hook,
packages retained archives, checksums and provenance and performs Store delivery.

The tests use fixtures, not live paid API calls. Authenticated installed-plugin,
Store artifact and real coding-agent acceptance remain separate release gates.
For an explicitly labeled local UI fixture, run `node tests/serve-fixture.mjs`.

## Upstream references

- [Typesafe API](https://docs.typesafe.ai/api)
- [Jev models and input limits](https://docs.typesafe.ai/models)

The initial adapter targets the API and Jev 1.13 documentation verified on
2026-09-25. Model changes require review of both the adapter and host catalog.
