# auditaz-cli

Open-source, **local, read-only** AWS cost data collector for
[Auditaz](https://auditaz.com).

`auditaz` runs on your machine using the AWS credentials you already have. It
makes only `Describe`/`List`/`Get` calls, writes what it found to a JSON file you
can inspect, and uploads it **only when you choose to**. The analysis that turns
that data into dollar savings and fix commands runs on the backend, after upload
— this program never computes prices, never suggests changes, and never modifies
your account.

## Why this exists

The most common objection to a cost tool is "why would I give an unknown company
access to my AWS account?" This tool exists so you don't have to. There is no IAM
role to deploy and no third-party access. You run it, you read the file it
produces, and you decide whether to send it.

> **Don't take our word for it — verify.** The whole point is auditability. Every
> release binary is built by GitHub Actions from a tagged commit and carries a
> provenance attestation you can check with one command, and the source it was
> built from is right here. Compiling it yourself stays a first-class path — see
> [Install](#install) and [Audit before you run](#audit-before-you-run).

## What it does — and does not — do

- **Does:** read-only `Describe`/`List`/`Get` calls to enumerate resources and
  usage metrics; write a JSON snapshot locally; optionally upload that snapshot.
- **Does NOT:** create, modify, tag, or delete anything in your account; read
  object contents (`s3:GetObject`), secret values, or any long-lived credential;
  phone home. The **only** two network calls in the entire program are the upload
  you confirm during `scan` and the `submit` command. `preview` is fully offline.

The complete list of API actions the collector issues is declared in
[`awsx/actions.go`](awsx/actions.go) and enforced read-only by a test
([`awsx/nonwrite_test.go`](awsx/nonwrite_test.go)) that checks every action
against the vendored AWS Service Authorization Reference. Any new call that isn't
classified read-only fails the build.

### API actions

```
ce:GetCostAndUsage
ec2:DescribeRegions, DescribeInstances, DescribeVolumes, DescribeSnapshots,
    DescribeAddresses, DescribeNatGateways, DescribeRouteTables
rds:DescribeDBInstances, DescribeDBSnapshots
s3:ListAllMyBuckets, GetBucketLocation, GetLifecycleConfiguration,
   ListBucketMultipartUploads, ListMultipartUploadParts
cloudfront:ListDistributions
cloudwatch:GetMetricStatistics
elasticloadbalancing:DescribeLoadBalancers, DescribeTargetGroups, DescribeTargetHealth
logs:DescribeLogGroups
dynamodb:ListTables, DescribeTable
redshift:DescribeClusters
route53:ListHostedZones
sts:GetCallerIdentity
```

## Install

Three ways in, in order of how little they ask of you.

### Download a binary

No Go toolchain, no clone, nothing installed outside the directory you are
standing in. Pick your platform from the
[latest release](https://github.com/auditaz/auditaz-cli/releases/latest), or:

```sh
# macOS, Apple Silicon
curl -fsSLO https://github.com/auditaz/auditaz-cli/releases/latest/download/auditaz_Darwin_arm64.tar.gz
tar xzf auditaz_Darwin_arm64.tar.gz auditaz
./auditaz scan
```

Swap `Darwin_arm64` for `Darwin_amd64` (Intel Mac), `Linux_amd64`,
`Linux_arm64`, or `Windows_amd64` (a `.zip`).

The binary stays where you unpacked it. Nothing is written anywhere else,
nothing needs `sudo`, and deleting the file removes every trace of the tool.

**Check it before you run it.** Every release artifact is built by GitHub
Actions from a tagged commit and carries a signed provenance attestation naming
that commit and the workflow run that produced it. Verify the archive before
unpacking — or the binary afterwards, both are attested:

```sh
gh attestation verify auditaz_Darwin_arm64.tar.gz --repo auditaz/auditaz-cli
```

Or compare the hash against `checksums.txt` from the same release:

```sh
shasum -a 256 -c checksums.txt --ignore-missing
```

### Build from source

Requires Go (see [`go.mod`](go.mod) for the version). Take this path if you
would rather run exactly what you just read. Building needs no network access
beyond the clone.

```sh
git clone https://github.com/auditaz/auditaz-cli.git
cd auditaz-cli
make install
auditaz scan
```

This installs to `$(go env GOPATH)/bin` (usually `~/go/bin`) for your user only:
no `sudo`, nothing outside your home directory, invisible to other accounts on
the machine. That directory isn't on everyone's `PATH` — if the shell answers
`command not found` right after a successful install, that's why, and
`make install` prints a note when it applies. Add it once:

```sh
echo 'export PATH="$HOME/go/bin:$PATH"' >> ~/.zshrc && source ~/.zshrc
```

**Local to the repository** — the binary stays in the clone:

```sh
make build
```

That compiles a single executable named `auditaz` into the repository root,
next to `go.mod`. Your shell doesn't look for commands in the current directory,
so every command in this README then needs a `./` in front of it:
`./auditaz scan`, `./auditaz preview <file>`. Nothing is written outside the
clone — delete the folder and no trace of the tool remains on your machine.

The examples in the rest of this README write `auditaz`; after a binary download
or a local build, read that as `./auditaz`.

## Audit before you run

Compiling it yourself is only half the point — the other half is checking what
you compiled. The read-only guarantee described above is a test, not a promise,
and it runs on your machine:

```sh
go test ./awsx/    # TestCollectorActionsAreReadOnly must pass
make test          # the whole suite
```

Two files answer most of what you'd want to verify:
[`awsx/actions.go`](awsx/actions.go) declares every API call the collector can
make, and [`cmd/auditaz/scan.go`](cmd/auditaz/scan.go) is the whole path from
collection to the upload prompt.

## Usage

One command does the whole thing:

```sh
auditaz scan
```

It collects the snapshot with your local AWS credentials across every enabled
region, writes `auditaz-scan-<timestamp>.json`, prints the same offline summary
`preview` would, and then — naming the file, so you can open it first — asks
whether to upload. Answer `n` and nothing leaves your machine; it prints the
`submit` line to use whenever you're ready. Answer `y` and it asks for the email
your report should go to. Pass `--email you@example.com` up front to skip that
question, or `--profile prod` to scan one specific account:

```sh
auditaz scan --profile prod --email you@example.com
```

The other two commands exist for the snapshot you already have — you don't need
them for the flow above:

```sh
# Read a snapshot again later — fully offline, no network, counts and facts only.
auditaz preview auditaz-scan-2026-09-08-142233.json

# Upload one you collected earlier, or declined to upload at the time.
auditaz submit auditaz-scan-2026-09-08-142233.json --email you@example.com
```

In a non-interactive shell (CI, a piped run) there is no prompt to answer, so
`scan` writes the file and stops. Uploading from there is explicit: `--yes`
together with `--email`.

Credentials come from the standard AWS chain: `~/.aws/credentials`, environment
variables (`AWS_ACCESS_KEY_ID`/`AWS_PROFILE`), or SSO — whatever you already use
with the AWS CLI. `--profile` is optional: it picks one named profile from your
shared config, which is handy for scanning a specific account without exporting
`AWS_PROFILE` first. Left unset, the default chain applies. The collector never
assumes a role and never reads a credential into the snapshot. If some calls are
denied by your permissions, the collector records which ones were skipped and
still produces a usable snapshot.

Every flag on `scan` is optional:

| Flag | Default |
|------|---------|
| `--profile <name>` | the default credential chain (`AWS_PROFILE`, env, SSO) |
| `--region <id>` | `AWS_REGION` / your profile, else `us-east-1` — bootstrap only |
| `--output <file>` | `auditaz-scan-<timestamp>.json`, never overwriting an existing file |
| `--email <addr>` | asked at the upload prompt, only if you answer yes |
| `--yes` | off — upload is never silent without it (and it requires `--email`) |

Region coverage is automatic: the collector calls `ec2:DescribeRegions` and scans
every region enabled on the account. `--region` only overrides the bootstrap
region that discovery starts from — useful for other partitions (`us-gov-west-1`,
`cn-north-1`) where the default endpoint isn't reachable. Left unset, the region
comes from your environment or profile (`AWS_REGION`, `~/.aws/config`), falling
back to `us-east-1`. If `ec2:DescribeRegions` is denied, the scan covers the
bootstrap region only.

### Inspect before you send

The snapshot is plain, indented JSON with human-readable keys. Open it in any
editor, or run `auditaz preview <file>`, to see exactly what would be uploaded.
Nothing leaves your machine until you run `submit` or confirm the prompt.

If you'd rather see the format before running anything,
[`docs/example-snapshot.json`](docs/example-snapshot.json) is a full snapshot
from a small fictional account. A test decodes it against the real `Snapshot`
type, so it can't drift from what the collector actually writes.

The upload destination is a fixed constant in the source
([`cmd/auditaz/main.go`](cmd/auditaz/main.go)) — there is no flag or
environment variable to redirect it, by design.

## Layout

```
awsx/        read-only AWS collection (Live interface + credential-chain client)
snapshot/    the JSON contract: Collect() + the Snapshot type
preview/     offline summary (counts/facts only — no pricing, no fix commands)
cmd/auditaz/ the CLI: scan / submit / preview
docs/        the snapshot format, by example
```

## License

Apache License 2.0 — see [LICENSE](LICENSE) and [NOTICE](NOTICE).
