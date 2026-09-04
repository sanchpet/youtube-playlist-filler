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
is never skipped — nor allowed to return a partial answer. A short read of the target playlist is
not a smaller answer, it is a wrong one: the ids it failed to reach are exactly the ids that would
then be added again.

## What a run does

1. Read the **whole** target playlist — `playlistItems.list`, 50 per page, following every
   `nextPageToken`.
2. Read each channel's uploads playlist the same way. Its id is the channel id with the leading
   `UC` replaced by `UU`; the playlist is auto-generated, ordered newest first, and lists
   everything the channel has ever published. A normal run takes **only the first page** — the 50
   newest, which is weeks of headroom at any cadence these channels publish at.
3. Subtract what the playlist already holds — **keyed on the video id alone**. Publication
   timestamps differ between endpoints for the same video and change when a video is edited, so any
   key involving them reports everything as new on every run.
4. Look up durations for what is left (`videos.list`, up to 50 ids per call).
5. Keep what falls inside a closed duration band, by default **15 minutes ≤ d ≤ 3 hours**.
6. Insert the survivors, without `snippet.position`.

`snippet.position` is omitted deliberately. Setting it declares the playlist manually ordered, and
YouTube then rejects every later insert that does not carry one, with `manualSortRequired`.

### There is one discovery path, and it is the API

An earlier design read each channel's Atom feed at `/feeds/videos.xml?channel_id=…` — free, no
credential, 15 entries. **That endpoint is gone.** It returns `404` for every channel id tested,
including controls whose channel pages return `200`, and it degraded mid-session: the same URL that
answered `200` with 15 entries from a logged-in browser tab returned `500` with cookies and `404`
without, minutes later. It is not egress, not cookies and not the ids.

The RSS path was removed outright rather than left behind a flag. A broken path that is merely
switched off is an invitation to switch it back on, and there is deliberately no
try-RSS-then-fall-back-to-the-API arrangement: two discovery paths means two failure modes and a
fallback that is only ever exercised during an incident.

### The band is now the only thing keeping Shorts out

With the feed went the `/shorts/` link hint — the one signal that named a Short as a Short. An
uploads playlist lists Shorts alongside everything else with nothing to distinguish them, so
**duration is the entire filter**, and the floor is the load-bearing half.

This is not hypothetical. A ceiling-only filter pulled **232 Shorts of 11–15 seconds** into this
playlist, 171 of them from a single channel — every one of them comfortably under three hours.
`TestInBandRejectsAtBothEnds` pins both bounds, and `TestRunKeepsShortsOutOfARealUploadsPage` runs
the whole reconcile over a fixture with the real distribution: 34 Shorts and 3 over-length uploads
out of 50 listed, 13 taken.

## `--full-reconcile`

Same purpose as before, different mechanism: it walks **every** page of every uploads playlist
rather than just the newest one. A normal run sees a bounded window and nothing tells it what fell
out the back of that window, so this is the pass that recovers a burst of publishing.

It belongs on a weekly schedule beside the normal one, not in place of it.

## Quota

The daily budget is 10,000 units and resets at midnight Pacific.

| Call | Units |
| --- | --- |
| `playlistItems.list` | 1 |
| `videos.list` | 1 |
| `playlistItems.insert` | **50** |

Against today's playlist (2134 items) and ten channels:

| Run | Cost |
| --- | --- |
| normal | 43 pages of the target playlist + 10 channel pages + ~1 `videos.list` ≈ **54 units** |
| `--full-reconcile` | ≈ **92 units** |

Both are noise. Reads are not worth economising on; the only meaningful spend is inserts, at 50
units each, which puts the hard ceiling at roughly **196 inserts a day**.

`search.list` is never called: it costs 100 units and draws on a separate daily allowance, and the
uploads playlist answers the same question for one unit.

`-max-inserts` (default 100) is the fuse. It exists so a bug in the diff — one that decides every
video is new — costs one capped run rather than the day. Each run logs `estimated_units`.

## Failure modes it handles by name

- **Playlist full.** A `403` with reason `playlistContainsMaximumNumberOfVideos` stops the run
  loudly and exits `0`. The limit is not documented and is not hard-coded here; the error is the
  contract. Retrying or crash-looping would only bury the line that says what happened.
- **Quota exhausted.** A `403` with reason `quotaExceeded` fails the run immediately and is never
  retried — the budget is gone until the reset, and every retry is another failure charged for.
- **Rate limited.** A `403` with reason `rateLimitExceeded`, and any `5xx`, are retried with
  jittered exponential backoff.
- **An unreadable channel** costs that channel's videos, not the run; the other channels are still
  reconciled, and the run still ends in an error so a channel failing all week is visible.

The first three arrive as HTTP 403 and are told apart only by the reason string in the body.

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

Durations are Go duration strings (`15m`, `3h`). A malformed value is an error rather than a
fallback to the default, and so is an inverted band — it matches nothing, so the run succeeds,
reports no candidates and looks exactly like a quiet week.

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
