# Plan: Optional SSH Key Generation

## Context

Currently the plugin requires users to manually:
1. Pre-configure SSH public keys in the instance group template metadata
2. Set `connector_config.key_path` in the runner config pointing to the private key

This adds operational overhead. The goal is to let the plugin optionally generate an ephemeral ED25519 key pair, inject the public key into the instance group template, and return the private key via `ConnectInfo()` — fully automating SSH key management.

## Files to Modify

| File | Change |
|---|---|
| `config.go` | Add `GenerateSSHKey bool` field |
| `sshkey.go` | **New file**: `generateED25519Key()` + `injectSSHKey()` method |
| `plugin.go` | Add `sshPrivateKey`/`sshPublicKey` fields to struct; call keygen in `Init()`; return key in `ConnectInfo()` |
| `plugin_test.go` | Add ~7 tests + `newTestGroupWithTemplate()` helper |
| `go.mod` | Add `golang.org/x/crypto` (for `ssh.NewPublicKey`, `ssh.MarshalAuthorizedKey`) |
| `README.md` | Document `generate_ssh_key` option |

## Implementation Steps

### 1. `config.go` — add config field

```go
GenerateSSHKey bool `json:"generate_ssh_key,omitempty"`
```

No validation changes needed — `false` is the correct default.

### 2. `sshkey.go` — new file with two functions

**`generateED25519Key() (privateKeyPEM []byte, authorizedKey string, err error)`**
- `crypto/ed25519.GenerateKey(crypto/rand.Reader)` — generate keypair
- `crypto/x509.MarshalPKCS8PrivateKey` + `pem.EncodeToMemory` — PEM-encode private key
- `ssh.NewPublicKey` + `ssh.MarshalAuthorizedKey` — format public key as authorized_keys line

**`(g *InstanceGroup) injectSSHKey(ctx, group) error`**
- Clone existing `group.GetInstanceTemplate().GetMetadata()` (preserve all keys)
- Build entry: `g.SSHUser + ":" + authorizedKey`
- Append to existing `ssh-keys` value (don't overwrite other users' keys)
- Call `g.client.Update()` with field mask `"instance_template.metadata"` and `InstanceTemplate{Metadata: merged}`

### 3. `plugin.go` — struct + Init + ConnectInfo changes

**Struct** — add two unexported fields:
```go
sshPrivateKey []byte
sshPublicKey  string
```

**Init()** — after group validation, before return:
```go
if g.GenerateSSHKey {
    privKey, pubKey, err := generateED25519Key()
    // store on g, call g.injectSSHKey(ctx, group)
}
```

**ConnectInfo()** — after setting Username:
```go
if g.sshPrivateKey != nil {
    info.ConnectorConfig.Key = g.sshPrivateKey
    info.ConnectorConfig.UseStaticCredentials = true
}
```

`UseStaticCredentials = true` tells the fleeting framework to use the plugin-provided key directly instead of looking at `connector_config.key_path`.

### 4. Tests to add

| Test | What it verifies |
|---|---|
| `TestGenerateED25519Key` | PEM is parseable, public key starts with `ssh-ed25519` |
| `TestInit_GenerateSSHKey_InjectsMetadata` | Update called with `ssh-keys` in metadata, existing metadata preserved |
| `TestInit_GenerateSSHKey_AppendsToExistingKeys` | Existing `ssh-keys` entries not overwritten |
| `TestInit_GenerateSSHKey_UpdateError` | Init fails if metadata update fails |
| `TestInit_GenerateSSHKey_Disabled` | No Update call when feature is off |
| `TestConnectInfo_WithGeneratedKey` | Key and UseStaticCredentials set in ConnectInfo |
| `TestConnectInfo_WithoutGeneratedKey` | Key stays nil when feature is off |

Helper needed: `newTestGroupWithTemplate(targetSize, metadata)` — extends mock group with `InstanceTemplate`.

### 5. Dependency

```
go get golang.org/x/crypto
```

Already a transitive dependency; needs to be promoted to direct.

## Tasks

### Iteration 1: Core — key generation and injection

- [x] `go get golang.org/x/crypto` — add dependency
- [x] `config.go` — add `GenerateSSHKey bool` field to `Config`
- [x] `sshkey.go` — create file with `generateED25519Key()` function
- [x] `sshkey.go` — add `injectSSHKey(ctx, group)` method on `InstanceGroup`
- [x] `plugin.go` — add `sshPrivateKey []byte` and `sshPublicKey string` fields to `InstanceGroup`
- [x] `plugin.go` — call keygen + inject in `Init()` when `GenerateSSHKey == true`
- [x] `plugin.go` — return private key in `ConnectInfo()` when `sshPrivateKey != nil`
- [x] Verify: `go build ./...` compiles without errors

### Iteration 2: Tests

- [x] `plugin_test.go` — add `newTestGroupWithTemplate(targetSize, metadata)` helper
- [x] `plugin_test.go` — add `TestGenerateED25519Key` (PEM parseable, key type `ssh-ed25519`)
- [x] `plugin_test.go` — add `TestInit_GenerateSSHKey_InjectsMetadata` (Update called, existing metadata preserved)
- [x] `plugin_test.go` — add `TestInit_GenerateSSHKey_AppendsToExistingKeys` (multi-key `ssh-keys`)
- [x] `plugin_test.go` — add `TestInit_GenerateSSHKey_UpdateError` (Init fails on Update error)
- [x] `plugin_test.go` — add `TestInit_GenerateSSHKey_Disabled` (no Update call when off)
- [x] `plugin_test.go` — add `TestConnectInfo_WithGeneratedKey` (Key + UseStaticCredentials set)
- [x] `plugin_test.go` — add `TestConnectInfo_WithoutGeneratedKey` (Key stays nil)
- [x] Verify: `go test ./... -v -timeout 60s` — all tests pass

### Iteration 3: Documentation and cleanup

- [x] `README.md` — document `generate_ssh_key` config option
- [x] `README.md` — add usage example with `generate_ssh_key: true`
- [x] `examples/config.toml` — add commented-out `generate_ssh_key` example
- [x] Verify: `go vet ./...` — no issues
- [x] Verify: `golangci-lint run ./...` — no lint warnings

## Verification

```bash
go test ./... -v -timeout 60s   # all existing + new tests pass
go vet ./...                     # no issues
go build ./...                   # compiles clean
```
