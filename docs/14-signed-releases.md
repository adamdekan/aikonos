# 14 — Signed Releases and SBOMs

> **Purpose.** Every tagged release of Aikonos ships its first-party services as
> container images that are signed, carry a software bill of materials (SBOM),
> and come with build provenance. This page covers what a release contains, what
> its signatures prove, how to verify one before you deploy it, how to deploy
> from it, and how a maintainer cuts one.
>
> The pipeline is [`.github/workflows/release.yml`](../.github/workflows/release.yml).
> Verification is [`scripts/verify-release.sh`](../scripts/verify-release.sh)
> (`task release:verify -- vX.Y.Z`).

---

## What a release contains

Six images, one per first-party service, listed in
[`.github/release-images.json`](../.github/release-images.json): `broker`,
`agent-gateway`, `webui`, `office-worker`, `docs-site` and `docs-mcp`. Each is
published as `ghcr.io/adamdekan/aikonos/<name>:<version>` for `linux/amd64` and
is always referenced by digest.

Attached to each image in the registry, as OCI referrers of its digest:

| Artifact | Format | Made by |
|---|---|---|
| Signature | Sigstore bundle | `cosign sign`, keyless |
| SBOM attestation | in-toto statement, CycloneDX 1.6 predicate, Sigstore bundle | `syft`, then `cosign attest` |
| Build provenance | SLSA v1 provenance, Sigstore bundle | GitHub artifact attestations (`actions/attest`) |

Attached to the GitHub release:

| File | What it is |
|---|---|
| `compose.release.yaml` | Compose overlay that runs every first-party service from its release image, pinned by digest, instead of building it |
| `images.txt` | The same image references, one per line |
| `aikonos-<name>-<version>.cdx.json` | CycloneDX 1.6 SBOM per image |
| `aikonos-<name>-<version>.spdx.json` | SPDX 2.3 SBOM per image, the same inventory |
| `aikonos-<version>-source.tar.gz` | The source tree the images were built from (`git archive` of the tag) |
| `SHA256SUMS` | Checksums of every file above |
| `SHA256SUMS.sigstore.json` | Keyless cosign signature over `SHA256SUMS` |
| `provenance.sigstore.json` | SLSA provenance covering every file in `SHA256SUMS` |

Third-party images (Postgres, Vault, NATS, Keycloak, OPA, OpenFGA and the rest)
are not rebuilt or re-signed. They stay pinned by digest in
[`deploy/compose/compose.digests.yaml`](../deploy/compose/compose.digests.yaml);
their SBOMs are their publishers'.

---

## What the signatures prove

Every signature and attestation in a release is made without a long-lived key.
At signing time, GitHub Actions issues the workflow run an OIDC token. Sigstore's
certificate authority exchanges it for a short-lived certificate naming that
workflow, and the signature is recorded in Sigstore's public transparency log.
Verification then pins one identity:

```
https://github.com/adamdekan/aikonos/.github/workflows/release.yml@refs/tags/<version>
issued by https://token.actions.githubusercontent.com
```

A signature that verifies against that identity proves the artifact was produced
by this repository's release workflow, running for that exact tag, and that the
signing was publicly logged. A signature made from a fork, a branch, another
workflow file, another tag, or anyone's laptop fails verification. The
provenance adds how the image was built: the commit, the workflow run, and a
GitHub-hosted runner (SLSA v1.0 Build Level 2).

What the signatures do **not** prove:

- **That the code is free of vulnerabilities.** The SBOM tells you what to scan;
  it is not a scan result.
- **That the image follows from the source alone.** The builds are not yet
  bit-for-bit reproducible, so a third party cannot rebuild an image and get
  the same digest. That is open work in [`ROADMAP.md`](../ROADMAP.md).
- **That the tag was pushed by the right person.** Anyone who can push a `v*` tag
  to this repository can start a release that verifies. Tag protection (below)
  is what limits that to maintainers.

---

## Verify a release

You need [cosign](https://docs.sigstore.dev/cosign/system_config/installation/)
3.x, and for the provenance check the [GitHub CLI](https://cli.github.com/).

### With the script

From a checkout of the tag:

```bash
scripts/verify-release.sh v1.2.3                 # signatures, SBOM attestations, checksums
scripts/verify-release.sh v1.2.3 --provenance    # and SLSA provenance (needs gh)
```

It downloads the release files into `./aikonos-v1.2.3/` (or `--dir`) and stops at
the first failure:

1. `SHA256SUMS` is signed by the release workflow identity for this tag.
2. Every release file matches `SHA256SUMS`.
3. `compose.release.yaml` pins exactly the images in `images.txt`, all tagged
   with this version and addressed by digest.
4. Every image has a signature and a CycloneDX SBOM attestation from the same
   identity.
5. With `--provenance`, every image's SLSA provenance verifies for the same
   identity and tag.

The release workflow runs the same script, with `--provenance`, against
everything it is about to publish, and publishes only if it passes.

### By hand

Without a checkout, or to see each check, run the commands the release notes
print, in a directory holding the release's files:

```bash
ID=https://github.com/adamdekan/aikonos/.github/workflows/release.yml@refs/tags/v1.2.3
ISSUER=https://token.actions.githubusercontent.com

cosign verify-blob --bundle SHA256SUMS.sigstore.json \
  --certificate-identity "$ID" --certificate-oidc-issuer "$ISSUER" SHA256SUMS
sha256sum -c SHA256SUMS   # macOS: shasum -a 256 -c SHA256SUMS

while read -r image; do
  cosign verify --certificate-identity "$ID" --certificate-oidc-issuer "$ISSUER" "$image" >/dev/null
  cosign verify-attestation --type cyclonedx \
    --certificate-identity "$ID" --certificate-oidc-issuer "$ISSUER" "$image" >/dev/null
done < images.txt

# Provenance, per image and for the release files:
gh attestation verify oci://<image> --bundle-from-oci --repo adamdekan/aikonos \
  --cert-identity "$ID" --source-ref refs/tags/v1.2.3
gh attestation verify aikonos-v1.2.3-source.tar.gz --bundle provenance.sigstore.json \
  --repo adamdekan/aikonos --cert-identity "$ID"
```

Use the exact `--certificate-identity`, not `--certificate-identity-regexp`. A
loose pattern such as `.*aikonos.*` also accepts forks.

### Air-gapped sites and registry mirrors

Verify on a connected machine, then copy the images into your registry by
digest. A digest is a hash of the content, so an image pulled by the same
digest from a mirror is the same image you verified. To check the copies
themselves, copy the referrers along with the images (for example `oras cp -r`)
and point the script at the mirror:

```bash
scripts/verify-release.sh v1.2.3 --image-prefix registry.example.org/aikonos
```

Set `AIKONOS_IMAGE_PREFIX=registry.example.org/aikonos` in `.env` and the
release overlay pulls the same digests from there.

---

## Deploy from a release

The overlay replaces the source builds of the six first-party services. Use it
with the compose files from the same tag, because an overlay and a
`compose.yaml` from different versions are not supported together.

1. Verify the release (above). That leaves the verified files in
   `./aikonos-v1.2.3/`.
2. Use the source tree of the same tag: either a checkout of the tag, or the
   verified source tarball (`tar -xzf aikonos-v1.2.3/aikonos-v1.2.3-source.tar.gz`).
3. Copy the overlay into it as `deploy/compose/compose.release.yaml`. Git
   ignores that path.
4. Add the overlay to the file list in `.env`, so every script that calls
   `docker compose` uses it too. For example, on-prem:

   ```bash
   COMPOSE_FILE=compose.yaml:deploy/compose/compose.onprem.yaml:deploy/compose/compose.digests.yaml:deploy/compose/compose.release.yaml
   ```

5. Pull and start: `docker compose pull && docker compose up -d`. Seeding and
   verification are unchanged; follow the guide for your variant
   ([on-prem](../deploy/onprem/README.md), [Azure](../deploy/azure/README.md)).

The webui reads its OIDC settings (`AIKONOS_WEBUI_OIDC_*`) from the container
environment at startup and serves them to the browser as `/runtime-config.js`,
so the same image works with Keycloak or Entra without a rebuild. A change to
one of them needs only `docker compose up -d webui`.

To upgrade, repeat the steps with the new version and its overlay.

---

## The SBOMs

Generated by [syft](https://github.com/anchore/syft) from each image as pushed,
scanned back from the registry by digest. They list packages, not files: Debian
packages, Go modules (read from the broker binary), npm packages, and the
office-worker's Python environment, each with version, licence where known, and
package URL (purl). Listing every installed file would add thousands of entries
per image and no further components; the signed digest already covers file
integrity.

CycloneDX is pinned to 1.6 and SPDX to 2.3, the versions current tooling reads.
To feed one to a scanner or to Dependency-Track:

```bash
grype sbom:aikonos-broker-v1.2.3.cdx.json
```

To take the SBOM from the registry instead of the release page, with its
signature checked on the way:

```bash
cosign verify-attestation --type cyclonedx \
  --certificate-identity "$ID" --certificate-oidc-issuer "$ISSUER" "$image" \
  | jq -r '.payload' | head -n 1 | base64 -d | jq '.predicate' > sbom.cdx.json
```

No vulnerability report or VEX ships with a release yet.

---

## Cutting a release

### Once per repository

1. **Protect release tags.** Add a tag ruleset for `v*` that restricts creating,
   updating and deleting tags to maintainers. The tag is part of the signing
   identity, so whoever can push one can produce a release that verifies.
2. **Turn on immutable releases** in the repository settings. A published
   release's assets and tag then cannot be changed.
3. **Make the packages public.** GHCR creates new packages as private. On the
   first release the workflow pushes and signs the images, then stops before
   publishing: a release whose images only the maintainer can pull is one
   nobody else can verify. Set each `aikonos/<name>` package's visibility to
   public (Package settings), then re-run the failed job.

### Each release

1. Optional dry run: **Actions → Release → Run workflow** on `main`. It builds
   every image, writes every SBOM, assembles and checks the release files, and
   pushes, signs and publishes nothing. The assembled files are kept as the
   run's `release-dry-run` artifact.
2. Tag and push. A signed tag is recommended; the on-prem deploy hook can
   enforce signed tags too.

   ```bash
   git tag -s v1.2.3 -m "v1.2.3"
   git push origin v1.2.3
   ```

3. The workflow builds, pushes, signs and attests the images, assembles and
   signs the release files, verifies all of it with
   `scripts/verify-release.sh --provenance`, checks the images are public, and
   publishes the GitHub release. A tag with a pre-release suffix (`v1.2.3-rc.1`)
   publishes a pre-release.

If a run fails after the images are pushed, nothing has been published as a
release yet. Fix the cause and re-run the failed jobs. Re-running signs again,
which is harmless: an image can carry more than one valid signature.

### Adding an image

A new first-party service that compose builds from source needs an entry in
[`.github/release-images.json`](../.github/release-images.json). CI enforces
this: `scripts/tests/release-scripts.test.sh` assembles a release from that list
and fails when any service outside the test-only `dev` profile would still be
built from source.

### Updating the pipeline's tools

The release workflow runs GitHub's own actions pinned by commit, BuildKit pinned
by digest, and the cosign and syft binaries pinned by version and SHA-256. To bump
cosign or syft, verify the new release with its publisher's signature first,
then copy the checksum into the `env:` block of `release.yml`:

```bash
# cosign: each binary is signed by the Sigstore project
cosign verify-blob --bundle cosign-linux-amd64.sigstore.json \
  --certificate-identity keyless@projectsigstore.iam.gserviceaccount.com \
  --certificate-oidc-issuer https://accounts.google.com cosign-linux-amd64

# syft: the checksum list is signed by Anchore's release workflow
cosign verify-blob --bundle syft_<version>_checksums.txt.sigstore.json \
  --certificate-identity https://github.com/anchore/syft/.github/workflows/release.yaml@refs/heads/main \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com syft_<version>_checksums.txt
```

Those two identities were correct for cosign v3.1.3 and syft v1.54.0. If a
publisher changes how it signs, confirm the new identity from their documentation.
Do not loosen the check to make it pass.

---

## Limits

- `linux/amd64` only. The broker build cross-compiles for amd64 explicitly.
- Not reproducible bit for bit yet (see above).
- Third-party images are pinned, not re-signed.
- No vulnerability report or VEX in a release yet.
