# Black Flag changes

This fork adds capacity options to the AWS provider so a runner launch does not
wait on one instance type in one subnet. All four are pool extra specs and are
off unless set, so an unchanged pool behaves exactly like upstream v0.1.6.

| Extra spec | Effect |
|---|---|
| `spot` | Launch as a one-time spot instance first. |
| `on_demand_fallback` | With `spot`, fall back to on-demand when no spot capacity is found. Default `true`. |
| `fallback_subnet_ids` | More subnets to try, in order, after `subnet_id`. |
| `fallback_flavors` | More instance types to try, in order, after the pool flavor. |

One `CreateRunningInstance` call walks spot placements (every subnet for each
flavor), then on-demand placements, and moves on only for capacity errors
(`InsufficientInstanceCapacity`, `MaxSpotInstanceCountExceeded`,
`SpotMaxPriceTooLow` and similar). Any other error stops at once. Each skipped
placement is logged to stderr, which GARM records.

Releases are tagged `v0.1.6-bfd.N` with the same asset names as upstream.

## v0.1.6-bfd.2

RunInstances capacity refusals bypass SDK retries so the next placement is tried immediately.
Throttling and other transient errors retain AWS's standard retry policy.
Recently refused placements are tried last within their market for three minutes;
none are excluded, and spot still precedes on-demand. AMI root device lookups
are cached for an hour, keyed by region, controller and image ID.

These caches persist across the provider's per-command processes using atomic,
private files. Set `cache_dir` in provider TOML to a directory writable by the
GARM service user (for example `/var/lib/garm/provider-cache`). Otherwise the
OS user cache directory is used. Missing, expired or corrupt entries and write
failures degrade to a normal lookup or the original placement order. Cached
entries contain no credentials or bootstrap data.

## v0.1.6-bfd.3

`early_bootstrap: true` adds a shared idempotence guard for the baked
`bfd-bootstrap-once` helper and starts the JIT runner without waiting on
sysinit/network.target. It requires a preconfigured Linux JIT image with
updates, extra packages, custom CA and preinstall scripts disabled. Without
the helper, cloud-init retains the normal bootstrap path.

`stopped_pool: "bfd-linux"` first claims a matching stopped reserve, updates
its user data while stopped, adds GARM's job tags, and starts it. Empty
reserves fall through to the usual cold launch. The reserve keeper in runner-os
owns unclaimed instances; GARM owns claimed instances and terminates them after
one job. Durable exclusive claim files and process locks in `cache_dir/claims`
prevent reuse across concurrent provider processes, stale EC2 listings and
provider crashes. Only a definitive capacity refusal permits another launch
after a start attempt. Runner-keyed records in `cache_dir/runner-claims` serialize
repeat creates and recover the same EC2 ID after a lost response. Retries describe
that instance, return it when pending/running, or resume its start when stopped;
an unknown state never causes a cold fallback. The lock also covers cold fallback:
its placement, EC2 idempotency token, request digest and returned ID are persisted.
A lost RunInstances response is recovered by token lookup or an identical replay
in the original placement; changed parameters or expired uncertain requests fail
closed. Claim JSON is atomically replaced under a separate permanent lock; records
contain no bootstrap data. Resumed reserves must still match the requested
plan, image, bootstrap digest and ownership. Cold recovery checks the current
request digest before token lookup, saved-ID recovery or replay, and rechecks
live ownership before returning any recovered instance. Named IAM profiles are fingerprinted distinctly from ARNs and
from other names, using the same encoding as the keeper.

The keeper and provider match a SHA-256 fingerprint of the image, allowed
placements, security groups, instance profile and disk configuration. Reserve
instances carry `GARM_CONTROLLER_ID`, `BFD_STOPPED_POOL`, and
`BFD_STOPPED_PLAN`, but never `GARM_POOL_ID` until claimed. Both processes run
on the single GARM host and share the persistent cache volume.
