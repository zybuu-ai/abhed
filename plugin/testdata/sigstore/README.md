# Public Sigstore test data

Real, unmodified artefacts from the public-good Sigstore instance, so the
offline verifier in `plugin/sigstore.go` is tested against what Fulcio and
Rekor actually issue, not only against `plugintest`'s stand-in. The tests read
them at a fixed time (2026-11-01) and never reach the network.

| File | Source | SHA-256 |
|---|---|---|
| `trusted_root.json` | https://raw.githubusercontent.com/sigstore/root-signing/main/targets/trusted_root.json (fetched 2026-11-17) | `6494e21ea73fa7ee769f85f57d5a3e6a08725eae1e38c755fc3517c9e6bc0b66` |
| `cosign_checksums.txt` | https://github.com/sigstore/cosign/releases/download/v3.1.4/cosign_checksums.txt | `e70e59b328d52949b3a9ae3e0d04721ea8f380c4e91c28197aa91df02afbf4c0` |
| `cosign_checksums.txt.sigstore.json` | https://github.com/sigstore/cosign/releases/download/v3.1.4/cosign_checksums.txt.sigstore.json | `8fc87b5562359b0414655ee281b80eb3752f58c4e137d5f0e4881f095bc26309` |

The bundle is a Sigstore bundle v0.3 that the cosign project's release
workflow made with `cosign sign-blob --bundle`, keyless, signed as
`keyless@projectsigstore.iam.gserviceaccount.com` (issuer
`https://accounts.google.com`). It has one Rekor v1 `hashedrekord` 0.0.1 entry
with a signed entry timestamp (log index 3341269697, integrated 2026-10-20).
