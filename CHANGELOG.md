# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.1.0] - 2026-10-21

First public release.

### Added

- Telegram bot that accepts a Zoom, Google Meet or Yandex Telemost link, sends an
  external meeting bot to the call, and returns a speaker-labeled transcript and an
  AI summary as files.
- Job lifecycle on PostgreSQL: the table is the queue, workers claim rows with
  `FOR UPDATE SKIP LOCKED`, and every status change is a guarded transition.
- Minute quota reserved when a job is created and refunded on failure.
- Swappable transcription providers (`mock`, `speech2text`, `mymeet`) and summarizers
  (`off`, `mock`, `openrouter`) behind small interfaces, chosen by environment variables.
- YooKassa payment links with fiscal receipts, status polling, idempotent payment
  handling and recurring payments with a self-service switch-off.
- "My calls" panel: cancel an active recording, re-deliver a transcript or a summary.
- Mock mode (`TRANSCRIPTION_PROVIDER=mock`) that needs no external keys, and an
  end-to-end dialog test (`TestDialog_MockHappyPath`) that prints the chat.
- GitHub Actions workflow that runs format, vet, lint, build and tests on PostgreSQL 16.

### Security

- The saved payment method token is erased when auto-renewal is switched off
  (migration `00015`).
- The app opens no inbound ports: Telegram long polling and status polling only.
