# Native T3 connection compatibility

Reviewed public T3 source on 16 September 2026: tag `v0.0.41-preview.20260916.1794` and main observed at `ccf220be205f0e509021dbc8cbda90daa638e20d`. These are source checks, not live-device tests.

## Supported recovery route

Direct URLs and T3 Connect do not grant SSH access. Updates need an explicitly enrolled SSH recovery route. Before preparation and installation, the original URL must return the same T3 environment ID and running server version as the SSH host. The tool checks the original URL again after installation. A public descriptor check proves endpoint reachability and version, not authenticated RPC or a Clerk session.

Explicit rollback can use a pinned SSH machine identity when the original route is down. A reachable route that identifies a different environment blocks rollback. The tool reports local recovery separately from failure to verify the original URL after recovery. Cleanup and saved status do not require that URL to be reachable.

Native self-update cannot satisfy retained backups. The launcher copies the stopped database, WAL, and SHM before the trial, restores them if the trial fails, and deletes its backup after success. The reviewed RPC contract has no retained-backup or later offline rollback operation. Native update mutations are therefore disabled in this updater. A device without a recovery route stays blocked.

Sources: [database backup and restore](https://github.com/pingdotgg/t3code/blob/v0.0.41-preview.20260916.1794/apps/server/src/serviceLauncher.ts#L114), [trial outcomes](https://github.com/pingdotgg/t3code/blob/v0.0.41-preview.20260916.1794/apps/server/src/serviceLauncher.ts#L538), [update contract](https://github.com/pingdotgg/t3code/blob/v0.0.41-preview.20260916.1794/packages/contracts/src/rpc.ts#L619).

## Public descriptor probe

`internal/native.Probe` reads `GET /.well-known/t3/environment`. It accepts credential-free HTTP(S) or WS(S) origins, with an optional `/ws` path. It rejects query strings, fragments, other paths, redirects, bad JSON, missing identity, and unexpected versions. Normal TLS checks and a ten-second timeout apply. No app tokens or keychain data are read.

The descriptor provides `environmentId`, `serverVersion`, `platform`, `orchestrationProtocolVersion`, and `capabilities`. There is no physical machine ID. Match environment IDs across routes and verify host identity separately. The relay broker URL is not the environment tunnel URL.

Sources: [descriptor route](https://github.com/pingdotgg/t3code/blob/v0.0.41-preview.20260916.1794/packages/contracts/src/environmentHttp.ts#L430), [descriptor schema](https://github.com/pingdotgg/t3code/blob/v0.0.41-preview.20260916.1794/packages/contracts/src/environment.ts#L184).

`t3 connect status --json` reports saved setup only: `desired`, `authenticated`, `linked`, `cloudUserId`, `relayUrl`, `publishAgentActivity`, and relay-client installation state. It does not check a live connection. See the [command](https://github.com/pingdotgg/t3code/blob/v0.0.41-preview.20260916.1794/apps/server/src/cli/connect.ts#L541) and its [printed warning](https://github.com/pingdotgg/t3code/blob/v0.0.41-preview.20260916.1794/apps/server/src/cli/connect.ts#L180).

The preview uses orchestration protocol 2; reviewed main uses 1. Never infer protocol compatibility from a shared package version. The launcher, self-update, auth, and saved Connect-status implementations were identical in the checked sources. See [preview protocol](https://github.com/pingdotgg/t3code/blob/v0.0.41-preview.20260916.1794/packages/contracts/src/environment.ts#L12) and [main protocol](https://github.com/pingdotgg/t3code/blob/ccf220be205f0e509021dbc8cbda90daa638e20d/packages/contracts/src/environment.ts#L12).

## T3 Connect enrollment

This updater does not implement relay OAuth enrollment or claim authenticated relay-session health. Existing desktop login records are not a credential export API.

A standalone relay client needs its own supported Clerk login, an active environment link, and its own DPoP key. The broker's `/v1/environments/:environmentId/connect` returns a one-time key-bound bootstrap credential. The client exchanges it directly with the environment. Application traffic then uses the tunnel hostname, not the relay Worker.

Sources: [T3 Connect design](https://github.com/pingdotgg/t3code/blob/v0.0.41-preview.20260916.1794/docs/internals/t3-connect.md), [relay endpoints](https://github.com/pingdotgg/t3code/blob/v0.0.41-preview.20260916.1794/packages/contracts/src/relay.ts#L1059), [proof checks](https://github.com/pingdotgg/t3code/blob/v0.0.41-preview.20260916.1794/infra/relay/src/http/Api.ts#L772).

## Local verification

Run `go test ./internal/native ./internal/host`. Native fixtures use local HTTP servers and host fixtures use a fake SSH executable. These checks do not contact live T3 installations or real relay accounts.
