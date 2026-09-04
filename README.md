# youtube-playlist-filler

Keeps one YouTube playlist stocked with long-form uploads from a fixed set of channels. Runs as a
Kubernetes CronJob: one shot per invocation, then exit. Most runs find nothing to do.

## It reconciles, it does not react

There is no database, no cursor and no seen-set. **The playlist is the state.** Every run reads it
in full, works out what is missing, and adds that. A run that was skipped, crashed halfway, or ran
twice leaves nothing to clean up — the next one simply finds a playlist that is missing fewer
videos.

That design is forced by one property of the API: **`playlistItems.insert` is not idempotent.**
Inserting a video that is already in the playlist returns `200` and creates a second item pointing
at the same video. Nothing upstream prevents duplicates, so the pre-read is not an optimisation and
is never skipped.

## What a run does

1. Read the whole target playlist (`playlistItems.list`, 50 per page, following `nextPageToken`).
2. Read each channel's Atom feed at `https://www.youtube.com/feeds/videos.xml?channel_id=UC…`.
   Free, no credential, 15 entries. Entries linking to `/shorts/` are dropped here.
3. Subtract what the playlist already holds — **keyed on the video id alone**. The `published` and
   `updated` timestamps differ between the feed and the API for the same video, and change when a
   video is edited, so any key involving them reports everything as new on every run.
4. Look up durations for what is left (`videos.list`, up to 50 ids per call).
5. Keep what falls inside a closed duration band, by default **15 minutes ≤ d ≤ 3 hours**.
6. Insert the survivors, without `snippet.position`.

`snippet.position` is omitted deliberately. Setting it declares the playlist manually ordered, and
YouTube then rejects every later insert that does not carry one, with `manualSortRequired`.

### Both ends of the band matter

The ceiling is the obvious half. The floor is the one that earns its keep: it is what keeps Shorts,
teasers and trailers out. A filter enforcing only the ceiling admits an eleven-second Short, since
eleven seconds is comfortably under three hours — which is exactly the defect this band exists to
prevent, and what `TestInBandRejectsAtBothEnds` pins down.

## `--full-reconcile`

The feed carries the 15 most recent entries and no watermark. A channel that published a sixteenth
video between two runs loses it silently and permanently. `--full-reconcile` enumerates each
channel's uploads playlist instead — the auto-generated id is the channel id with `UC` swapped for
`UU` — and runs the same diff.

It costs quota (one unit per 50 videos per channel, every run) where the feed costs nothing, so it
belongs on a weekly schedule beside the hourly one, not in place of it.

## Quota

The daily budget is 10,000 units and resets at midnight Pacific.

| Call | Units |
| --- | --- |
| `playlistItems.list` | 1 |
| `videos.list` | 1 |
| `playlistItems.insert` | **50** |

So the ceiling is roughly **196 inserts a day**, and everything else is noise. `search.list` is
never called: it costs 100 units, draws on a separate daily allowance, and the feed answers the
same question for free.

`-max-inserts` (default 100) is the fuse. It exists so that a bug in the diff — one that decides
every video is new — costs one capped run rather than the day. Each run logs `estimated_units`.

## Failure modes it handles by name

- **Playlist full.** A `403` with reason `playlistContainsMaximumNumberOfVideos` stops the run
  loudly and exits `0`. The limit is not documented and is not hard-coded here; the error is the
  contract. Retrying or crash-looping would only bury the line that says what happened.
- **Quota exhausted.** A `403` with reason `quotaExceeded` fails the run immediately and is never
  retried — the budget is gone until the reset, and every retry is another failure charged for.
- **Rate limited.** A `403` with reason `rateLimitExceeded`, and any `5xx`, are retried with
  jittered exponential backoff.

All three arrive as HTTP 403 and are told apart only by the reason string in the body.

## Configuration

Flags win over environment; the environment is what the CronJob sets.

| Env | Flag | Default |
| --- | --- | --- |
| `YT_PLAYLIST_ID` | `-playlist` | `PLbkjH9B2w9JzHthzAcmAnJ4N7ymUQOyOC` |
| `YT_CHANNEL_IDS` | `-channels` | the ten channels in `internal/config` |
| `YT_MIN_DURATION` | `-min-duration` | `15m` |
| `YT_MAX_DURATION` | `-max-duration` | `3h` |
| `YT_MAX_INSERTS` | `-max-inserts` | `100` |
| `YT_DRY_RUN` | `-dry-run` | off |
| `YT_FULL_RECONCILE` | `-full-reconcile` | off |
| `YT_CLIENT_ID` | — | required |
| `YT_CLIENT_SECRET` | — | required |
| `YT_REFRESH_TOKEN` | — | required |

`-dry-run` does everything except the inserts and logs what it would have added.

## Auth

OAuth2 for one personal account, scope `https://www.googleapis.com/auth/youtube`. The service holds
a refresh token and mints access tokens from it; there is no interactive flow in the service.

Obtaining the refresh token is a separate one-shot command, run once on a laptop:

```sh
YT_CLIENT_ID=… YT_CLIENT_SECRET=… go run ./cmd/enroll
```

It opens a loopback redirect on `127.0.0.1` (an ephemeral port — a Google *desktop* client is
allowed any of them, so nothing needs registering) and prints the refresh token to stdout, ready to
pipe into a secret store. The out-of-band copy-and-paste flow is retired and is not used.

Re-running it re-consents: Google issues a refresh token only on the first consent for a client, so
the request forces the prompt. If it ever prints no token, revoke the app at
<https://myaccount.google.com/permissions> and run it again.

## Development

```sh
mise run ci     # lint, test, build
mise run test
mise run lint
```

Branch and open a PR; the PR title is a Conventional Commit, because squash-merge makes it the
release commit.
