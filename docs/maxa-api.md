# Maxa fork API and release contract

All routes below are under `/api`; legacy root-level routes have the same behavior. Existing bearer-token/session authentication applies. Only health, version and login are public; capabilities, platform, session status/logout and WebSocket access require authentication. `ZENNOTES_VAULT_PATH` locks the served vault and makes vault selection return 409. Maxa deployment supplies its bind address and credentials; insecure/no-auth mode is not enabled by this fork.

## Metadata, visibility and files

`NoteMeta` retains `title` as the filename stem and adds:

- `displayTitle`: trimmed frontmatter YAML string `title`, falling back to the filename stem. Numbers, booleans, lists and empty strings do not become display titles.
- `aliases`: frontmatter string aliases (scalar or list), otherwise `[]`.
- `type: "canvas"` for `.canvas`, `type: "excalidraw"` for drawings.

`GET /notes` includes canvases. `updatedAt` is file mtime in milliseconds and supplies Recent; there is no separate Recent endpoint. Canvas text nodes feed text search; scene JSON is not indexed as Markdown tags/tasks. `POST /canvas/create {folder,title,subpath}` creates an empty JSON Canvas. Existing read/write/rename/move/trash/restore/duplicate operations retain the `.canvas` extension. Raw canvas reads/writes do not reserialize JSON, preserving unknown fields, numeric values and array order.

`GET|POST /vault/settings` round-trips `excludedPaths: string[]` (default `[]`) and `projectPathPrefix: string` (default `""`). A nonempty prefix must be relative, slash-terminated and have no empty/dot/parent components, backslashes, colon or control characters; invalid saves return 400. Maxa uses `.maxa/`.

Exclusion patterns are vault-relative and ordered: `*` matches within a component, `**` across components, `**/` also matches zero directories, trailing `/` matches a directory and descendants, and leading `!` re-includes. Last match wins independently for each path. Walkers traverse excluded directories; an excluded folder with visible descendants is listed as a folder. Dot directories remain hidden from listings. Exclusions apply to notes, assets, folders, text search, tags/tasks (including `includeExcluded` task scans), graph sources/targets, pending comments and watcher events. Access to excluded sources/destinations returns 404, without a content mutation. Recursive folder mutations also reject hidden descendants rather than moving/deleting them incidentally. Settings changes apply without restart.

## Versioned saves

`GET /notes/read?path=<vault-path>` returns metadata, raw `body` and `version`, the lowercase SHA-256 hex digest of the file bytes. Applies to Markdown and JSON Canvas, and to the existing arbitrary-file read capability.

`POST /notes/write`:

```json
{"path":"note.md","body":"replacement","baseVersion":"<read version>","force":false}
```

Success returns metadata plus the new `version`. A mismatching supplied base returns 409 with no write:

```json
{"code":"VERSION_CONFLICT","version":"<current version>","body":"<current text>"}
```

For an absent file the current version is `""`. Omitting `baseVersion` retains legacy overwrite behavior. `force:true` explicitly bypasses the base check. `zn write` always reads a base and sends it; `zn write --force` sends `force:true`. Incremental remote CLI append/prepend/task-toggle recompute against current content and retry version conflicts up to five attempts; persistent conflicts fail without forcing. Unsupported older servers fail rather than silently falling back to unversioned writes.

## Server-owned graph

- `GET /links/outgoing?path=`: references from the named source.
- `GET /links/backlinks?path=`: references resolving to the named target.
- `GET /links/unresolved`: missing and ambiguous references.
- `GET /links/orphans`: `[{"path":"…"}]`, live notes/canvases without another file's resolved inbound reference.

Every reference has these keys, including explicit nulls/empty arrays:

```json
{"source":"Source.md","line":4,"nodeId":null,"kind":"wikilink","raw":"target#Heading","anchor":"#Heading","status":"resolved","path":"research/target.md","candidates":[]}
```

`kind` is `wikilink|embed|markdown|canvas`; `status` is `resolved|missing|ambiguous`. Missing/ambiguous `path` is null; ambiguous `candidates` is sorted. Canvas references have null `line`, a `nodeId`, and come from each file node's `file`/`subpath`. Markdown lines are 1-based, including frontmatter. `raw` is the target including its anchor, excluding wikilink display alias and surrounding syntax. `anchor` includes `#` (or the canvas subpath), or is empty.

Extraction ignores frontmatter, fenced/inline code and external URLs. Wikilinks and embeds preserve `|display alias`, headings and block anchors. Inline Markdown destinations are URL-decoded and resolved relative to the source folder, or from the vault root for `/…`; no basename fallback is applied to a relative Markdown path.

Wiki/canvas resolution: slash-containing targets first match exact vault paths (optional `.md`), then unique path suffixes; bare targets match case-insensitive Markdown stems (optional `.md`) or full non-Markdown filenames; finally aliases are considered. Ambiguity at the first matching step is reported, never guessed. Trash/excluded files are neither sources nor targets. An immutable in-memory index serves queries. Each request checks visible file stats, pruning excluded subtrees unless a later re-include can intersect them; only added/changed files are reparsed and removed files are dropped. Linux change-time and inode identity also detect external edits with preserved size/mtime; recently changed files are byte-checked across coarse timestamp ticks without reparsing unchanged content. API writes and watcher events invalidate/update the same index. Concurrent requests share one refresh, which never takes the note read/write lock; freshness does not depend on watcher delivery. Non-Linux hosts retain byte checks when change-time is unavailable.

`POST /notes/rename`, `/notes/move` and `/folders/rename` add `changedFiles: string[]` to their existing results. Asset rename/move use the same reference authority. Rewrites preserve kind, display alias and anchor; path forms remain paths, Markdown paths remain relative, and bare targets stay bare only if unique after the move. Moving a Markdown source also rebases its resolved relative outgoing links. Canvas rewrites preserve unknown JSON fields and node/edge order. Partial rewrite failures return 500:

```json
{"code":"REWRITE_FAILED","unrewrittenFiles":["source.md"],"changedFiles":["other.md"]}
```

The rename/move itself may already have happened; reload state and repair the reported files. It is never reported as success. A per-vault mutation lock serializes API versioned writes with reference-preserving relocations; rewrites also compare file versions before publication. External processes do not participate in that lock: filesystem edits must still be coordinated across applications, particularly the small final compare/atomic-rename window.

### Shared fixture expectations

The executable fixture is `internal/vault/maxa_test.go`.

With `research/target.md` (alias `nickname`), `a/dup.md`, `b/dup.md` and `source.md`:

| Reference in source | Expected |
| --- | --- |
| `[[target]]` | resolved `research/target.md` |
| `[[research/target\|Label]]` | same target, alias preserved |
| `[[target#Section]]` | same target, anchor `#Section` |
| `![[target#^block]]` | embed, same target, anchor `#^block` |
| `[[nickname]]` | same target via alias |
| `[link](research/target.md#section)` | Markdown, same target, anchor `#section` |
| `[[dup]]` | ambiguous, candidates `a/dup.md`, `b/dup.md` |
| `[[absent]]` | missing, null path, empty candidates |
| Canvas file node `file=research/target.md`, `subpath=#Section` | canvas, null line, node id retained |
| A reference inside frontmatter, inline backticks or a fenced block; an HTTPS link | no reference |
| A reference targeting an excluded/trashed file | missing |

If renaming `research/target.md` to `renamed.md` while `other/renamed.md` exists, a resolved `[[target\|Label]]` becomes `[[research/renamed\|Label]]`, not an ambiguous bare link. A pre-existing ambiguous reference is not guessed or rewritten. From `research/local.md`, `[relative](target.md)` follows the target rename/move and is rebased again if the source moves.

## Comment queue and atomic CLI mutations

`GET /comments/pending?author=Maxa&wait=true&timeout=30` returns:

```json
[{"path":"note.md","threadId":"root-id","anchorText":"selected text","anchorLine":2,"anchorFound":true,"messages":[{"id":"root-id","author":"User","body":"Please change this","createdAt":1700000000000}]}]
```

Only unresolved threads whose latest message is not authored by the specified name appear, ordered by latest-message creation time, oldest first. All messages are included. An absent anchor has `anchorFound:false` and a best-effort line from its stored offset. `wait=true` polls on-disk state, including when watchers are disabled. `timeout` is 0..86400 seconds; omitted/zero means wait until a result or request cancellation. A positive timeout returns `[]`, HTTP 200. Storage stays `.zennotes/comments/<note>.comments.json`.

Additional route `POST /comments/mutate` provides atomic server-side read/modify/write for remote CLI operations:

```json
{"path":"note.md","operation":"add","comment":{"id":"new-id","body":"message","author":"Maxa","anchorText":"selection","anchorStart":0,"anchorEnd":9}}
```

`operation:"reply"` takes `comment.parentId` naming the root thread. `operation:"resolve"` takes `id` and `resolved:true|false`. Returns the current complete comment array; invalid mutations return 400. The server serializes sidecar mutations and assigns creation/update timestamps. CLI preserves the existing add/reply/resolve output shapes. The legacy full-array `/comments/write` route remains for compatibility; consumers needing incremental safety must use `/comments/mutate`.

Remote CLI commands:

```sh
zn --server http://127.0.0.1:7878 links note.md --json
zn --server http://127.0.0.1:7878 backlinks note.md --json
zn --server http://127.0.0.1:7878 unresolved --json
zn --server http://127.0.0.1:7878 orphans --json
zn --server http://127.0.0.1:7878 comment pending --author Maxa --wait --timeout 30 --json
```

Token input: `ZENNOTES_REMOTE_TOKEN`. Graph JSON is returned from the server index, not recomputed by the CLI. Remote `list`, `read --meta` and `search-title` JSON include `displayTitle`/`aliases`; title search matches filename title, display title and aliases. Local vault mode retains its upstream storage behavior and is not the Maxa access path.

## Translation

`GET /translate/status?path=note.md` validates the saved source and reads only its derived cache. A hit returns `{cacheHit:true,state:"translated",cachePath,body}`; a miss returns `cacheHit:false` with `state:"translating"` while a job runs, `state:"failed"` plus `error:{code,message}` after its last failure, or `state:"untranslated"` otherwise. Last failures are held in memory per cache key until the next start or server restart. It resolves configuration (including the target language used in the cache key), but never contacts the provider, reads a separate key file, requires a key or writes. An unreadable cache returns `CACHE_UNREADABLE`.

`POST /translate {"path":"note.md"}` returns `{cacheHit,cachePath,body}`. `cachePath` is vault-relative. Allowed extensions: `md, markdown, txt, rst, adoc`. Cache files and excluded source paths are rejected before provider/write effects. Both translation endpoints may read exactly the cache file derived from a validated source even when `translate-cache/` is excluded; POST may atomically create that file. Listings, search and every other API retain exclusions. Cache symlinks are rejected. Provider settings are resolved separately on **every translation and cache-status request**: explicit environment override > machine translation file > defaults. File edits apply on the next request, without restarting. Maxa owns syncing this file; the server only reads it.

`POST /translate {"path":"note.md","async":true}` validates the source/configuration and credentials using the same errors as synchronous POST. A cache hit returns 200; otherwise it starts or joins one job per cache key and returns 202 with `{cacheHit:false,state:"translating"}`. The job survives request disconnects, page reloads and browser timeouts. Without `async`, POST waits for the same job and keeps its existing response fields. Web polls cache status about every 2 seconds while translating (including after reload); only the initiating click opens the finished translation panel automatically.

The file path is `ZENNOTES_TRANSLATE_CONFIG_FILE` when set; otherwise `${XDG_STATE_HOME:-$HOME/.local/state}/maxa/zennotes/translate.json`. A missing file contributes no settings. An unreadable file, invalid JSON or invalid field returns `TRANSLATE_CONFIG_INVALID` before provider/network/write effects, including when a cache exists. Errors name the field but never echo values or file contents.

```json
{"version":1,"preset":"light_text","endpoint":"https://api.deepseek.com/chat/completions","apiKey":"<secret>","model":"deepseek-flash","temperature":0,"targetLanguage":"简体中文","extraBody":{}}
```

- `version` must be `1`; `endpoint`, when supplied, must be an absolute HTTP(S) URL.
- `apiKey`, `model`, and `targetLanguage`, when supplied, must be non-empty strings. `temperature` must be a finite number; `preset` is an optional informational string.
- `extraBody` must be an object. Its fields are merged into the Chat Completions request, but never override `model`, `messages`, `stream`, or `temperature`.
- The entire file is validated even if environment settings override some fields. A cache read needs no key; a configured separate key file is read only on a provider cache miss.

| Environment override | File field | Default |
| --- | --- | --- |
| `ZENNOTES_DEEPSEEK_API_KEY_FILE`, then `ZENNOTES_DEEPSEEK_API_KEY` | `apiKey` | no key |
| — | `endpoint` | `https://api.deepseek.com/chat/completions` |
| `ZENNOTES_TRANSLATE_MODEL` | `model` | `deepseek-flash` |
| `ZENNOTES_TRANSLATE_TARGET_LANGUAGE` | `targetLanguage` | `简体中文` |
| `ZENNOTES_TRANSLATE_TEMPERATURE` | `temperature` | `0` |

Unset environment variables inherit the file/default value. Explicit temperature `0` overrides a nonzero file value; malformed, empty or non-finite temperature is a configuration error, not silent zero. Empty model/language/config-path overrides are also errors. An explicitly empty key/key-file override clears credentials rather than falling back (cache reads still work; an uncached translation returns `MISSING_API_KEY`).

Credentials are neither serialized into host/vault settings nor exposed to clients or provider-error logs. Calls use non-streaming POST to the resolved `endpoint`, preserving Markdown/code/paths/identifiers and taking `choices[0].message.content`. Requests have a 30-minute deadline. Publication requires non-empty content, `choices[0].finish_reason == "stop"`, and matching ATX heading/fenced-code-block counts (headings inside fences do not count; fence closure must also match). Any mismatch returns `TRANSLATION_INCOMPLETE`, with safe numeric token/structure diagnostics; HTTP 200 or `stop` alone is not evidence of completeness. Empty/malformed responses retain their existing errors. Only that body is atomically published, with no cache metadata or wrapper added. Simultaneous identical-key requests are coalesced. A readable cache hit needs no key/network; an existing unreadable cache is not overwritten. Failures publish no cache file.

Errors are JSON `{code,message}`: `UNSUPPORTED_FILE` (400), `MISSING_API_KEY` (503), `UPSTREAM_ERROR` (502), `CACHE_UNREADABLE` (500), `TRANSLATE_CONFIG_INVALID` (500), `EMPTY_RESPONSE` (502), `TRANSLATION_INCOMPLETE` (502). Excluded path access is 404.

Cache normalization equals `lua/maxa/session/buffer_translate.lua`: normalize CRLF/CR to LF; remove each line's trailing spaces/tabs; trim ASCII whitespace; collapse 3+ LF to 2. Hash normalized UTF-8 bytes with SHA-256, then hash `projectPathPrefix + vaultPath + "\n" + semanticHash + "\n" + targetLanguage`. Cache file: `translate-cache/<key>.md`.

Reference-generated vectors (`TestMaxaTranslationCacheVectors`):

| Relative path | Input (JSON escaped) | Language | Cache key |
| --- | --- | --- | --- |
| `.maxa/notes/a.md` | `"  Alpha \t\r\nBeta\t \r\r\rGamma  \n "` | 简体中文 | `eb9d5a86ee372c11c98fe981634453f1d21f9d7335af216ef94682a27b82b977` |
| `.maxa/notes/你好.md` | `"# 标题\n\n\n\n正文\t \r\n"` | English | `6d6e338654a25b5406df74307860b55053f95bf95fee80b51d88b8f24e3944ec` |
| `empty.md` | `"\r\n \t\r"` | 简体中文 | `0baceca4bf6e472d8998bc4dcb36b4808236ad42c4dc38857d097ec4c96519d3` |

## Fork release assets and build provenance

Repositories: `maxzhao/znserver`, `maxzhao/zennotes`, `maxzhao/tui`; matching `ZenNotes/*` repositories remain upstream remotes for manual merges only. Versions are `X.Y.Z-maxa.N` (tags `vX.Y.Z-maxa.N`). Default versions: server `2.77.0-maxa.2`, CLI `0.11.0-maxa.1`.

| Product | Linux assets | SHA-256 file |
| --- | --- | --- |
| Server | `zennotes-server-linux-amd64`, `zennotes-server-linux-arm64` | `SHA256SUMS` |
| CLI | `zn_<version>_linux_amd64.tar.gz`, `zn_<version>_linux_arm64.tar.gz` (containing `zn`) | `checksums.txt` |

Manual release workflows require reviewed full source commit hashes and fork-suffixed tags and create drafts only. Server release additionally checks out a full `maxzhao/zennotes` `web_commit`, builds with Node 24/npm, verifies/re-attests the locally built artifact with `scripts/pin-fork-web.mjs`, runs `cmd/prepare-web`, then compiles with `-tags=embed_web`. The release includes `manifest.json` recording the fork Web commit, lockfile hash, file inventory and archive hash. No upstream Web archive URL is retained. The committed `web-artifact/manifest.json` pins a locally rebuilt fork Web artifact and has no remote archive URL. CI rebuilds that source from `maxzhao/zennotes`; a local/Docker build needs the matching local archive generated first. Plain local `go build ./cmd/zennotes-server` remains API-only. Local binaries built with `-tags=embed_web` serve the Web commit recorded in `web-artifact/manifest.json`.

All CLI native update and server install/update lookups use only `maxzhao/*`. Unsuffixed upstream pins are rejected; discovery filters fork tags and compares `N` numerically. Assets are checked against GitHub SHA-256 digests or the named checksum file before replacement. Retained server binaries used for rollback are checked against stored SHA-256 digests. `zn server install` supports Linux/WSL only. Package-manager/Go/desktop-owned CLI updates do not invoke an upstream installer. The inherited Homebrew formula is disabled until fork checksums are regenerated; the upstream Docker publisher is disabled in the fork. Optional Nix packaging is not validated and its CI job is disabled in the fork pending a reviewed vendorHash. No workflow merges or pushes back upstream automatically.

Focused checks live in `internal/vault/maxa_test.go`, `internal/httpserver/maxa_test.go`, `internal/watcher/maxa_test.go` and the CLI backend/release tests. Translation provider tests use only `httptest`, never DeepSeek. Web interaction and human review are separate acceptance gates; this API implementation does not establish them.
