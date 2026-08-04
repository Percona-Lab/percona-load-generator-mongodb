# Changelog

This file is intended to mirror the release notes you publish on GitHub.
For each new release, add a new `## vX.Y.Z` section at the top. The release workflow will use the first versioned section as the GitHub Release body when you push a `v*` tag.

---

## v4.6.0 - TLS Client Configuration & Metrics Accuracy

**Highlights**

**New: TLS Client Configuration**
- Added first-class MongoDB TLS client settings for `tlsCAFile` and `tlsCertificateKeyFile`.
- TLS is now enabled automatically when both TLS client files are provided, and disabled when they are not.
- The Web UI now uses file picker inputs for selecting TLS certificate files instead of plain text fields.
- Uploaded certificate files are saved to temporary paths and wired into the active connection configuration.

**Fixes**
- Fixed the live throughput graph so it uses actual elapsed time between samples instead of assuming perfectly timed polling.
- This removes inflated spikes in the final chart when browser timing drifts near the end of a run.

**Testing & Docs**
- Added tests covering TLS config validation, MongoDB URI generation, and TLS file upload handling.
- Updated `config.yaml` and `README.md` to document the new TLS fields and usage.

---

## Release Template

Copy the structure below for the next release and replace the version/title:

## vX.Y.Z - Release Title

**Highlights**

**New: Feature Area**
- Bullet one.
- Bullet two.

**Fixes**
- Bullet one.

**Testing & Docs**
- Bullet one.
