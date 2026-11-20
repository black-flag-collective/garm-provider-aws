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

Capacity refusals bypass SDK retries so the next placement is tried immediately.
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
