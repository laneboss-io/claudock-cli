# Changelog

## 0.2.0 - 2026-11-09

Adds Google Vertex AI profiles. `claudock profile add NAME --vertex --project
PROJECT_ID --region REGION` creates a profile that launches Claude Code against
Vertex AI using Application Default Credentials. Add `--adc ABS_PATH` to pin a credentials file for that profile
(exported as `GOOGLE_APPLICATION_CREDENTIALS` at launch). Like Console profiles, Vertex
profiles take a local budget with `claudock profile set-credit NAME AMOUNT`, and
`claudock usage` and `claudock available` show the budget left. The budget is a
local estimate: Go CLI launches do not yet record spend (for Console profiles
either), and it does not enforce a Google Cloud quota or spending cap.

## 0.1.0 - 2026-10-14

Claudock is a command line tool for using several Claude Code accounts as
profiles. Manage and sign in to profiles, launch Claude Code with
`claudock run PROFILE -- ...`, and read each profile's limits with
`claudock usage` to choose an account with room left. This first Go release
provides static binaries for macOS and Linux on amd64 and arm64, checksum
verification for downloads, and `claudock --version` to identify the release.
