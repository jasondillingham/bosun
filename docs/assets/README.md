# docs/assets/

Demo assets for the README and onboarding paths.

## bosun-tour.cast

Asciinema recording of `BOSUN_TOUR_AUTO=1 bosun tour` at 100x30 in
headless mode (no TTY needed; the auto-mode env var skips the
keypress waits). End-to-end: sandbox setup → init → simulated
edits → status → predict → merge × 2 → cleanup → teardown.

**Hosted player:** https://asciinema.org/a/aPMDJsNbseBdi307

**Local playback:**

```sh
asciinema play docs/assets/bosun-tour.cast
```

## Re-recording the tour

When the tour's flow changes, re-record with:

```sh
go build -o ~/go/bin/bosun ./cmd/bosun     # rebuild bosun first
cd /tmp                                     # any non-iCloud path
BOSUN_TOUR_AUTO=1 asciinema rec --cols 100 --rows 30 \
  --command "~/go/bin/bosun tour" \
  --overwrite docs/assets/bosun-tour.cast
```

The asciinema 3.x install-id auth lives at
`https://asciinema.org/connect/<uuid>` — printed by
`asciinema auth` when the CLI needs association. Visiting the
URL while signed in to asciinema.org links the CLI's install
identity to the account; after that, `asciinema upload <cast>`
works without further prompts.

## Re-uploading after a recording change

```sh
asciinema upload docs/assets/bosun-tour.cast
```

The upload returns a new URL with a fresh short code. Update the
README's asciinema link reference to the new code.

## Regenerating demo.gif from the cast

`demo.gif` lives in the repo root because GitHub renders inline
GIFs in README.md but doesn't embed asciinema players. Convert the
cast to GIF with `agg`:

```sh
brew install agg              # one-time install
agg docs/assets/bosun-tour.cast demo.gif
```

Default theme/dimensions are reasonable for a README hero. If you
want a smaller file, pass `--cols 80 --rows 24` or `--fps-cap 8`
to drop the rendering rate.

## Why a tracked cast file

The cast file is intentionally in the repo (5KB; not a binary blob
problem) so the demo doesn't depend on asciinema.org's uptime, the
operator's auth state, or someone else's account. The hosted player
is a UX win; the cast file is the durable artifact.
