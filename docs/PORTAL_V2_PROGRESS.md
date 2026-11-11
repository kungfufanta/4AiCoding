# PORTAL — TIẾN ĐỘ GIAI ĐOẠN 2 (P1–P6)

File theo dõi cho kế hoạch [`docs/PORTAL_PLAN_V2.md`](PORTAL_PLAN_V2.md). Cập nhật **mỗi PR**: đổi cột Trạng thái, thêm một dòng vào Nhật ký. Đọc file này trước khi bắt đầu milestone kế.

## Trạng thái milestone

| M | Nội dung | Trạng thái | PR | Ghi chú |
|---|---|---|---|---|
| P0 | Xiết chặt đăng nhập (bỏ mật khẩu mặc định) | ✅ Xong | #34 | Claude khác làm (main `1bda85c`) |
| Kế hoạch V2 | `docs/PORTAL_PLAN_V2.md` | ✅ Xong | #33 | Docs-only, CI xanh |
| P1 | Design system + app shell + migrate Login/Projects/Members/Audit + trang accept-invite + CSP + preview workflow | ✅ Xong | #36 | Merged 07:27 |
| **P2** | API key + agent token (S2 + UI) | ✅ Xong | #37 + #38 | Merged; #38 là fix bảo mật của Claude |
| P3 | Tag tenant + webhook ở sandboxd (S3a) | ✅ Xong | #40 | Merged; CI 7/7 xanh |
| P4 | Gateway sandboxd + trang Sandboxes (S3 + UI) | 🟨 Đang review | #46 | Branch `portal-p4-gateway-sandboxes` (z.ai) — gateway /v1 + webhook + UI list/detail + preview có sandboxd thật |
| P5 | Quota + rate limit + trang Usage (S4 + UI) | ⬜ | | Cần P4 |
| P6 | SDK + MCP + trang Quickstart (S5 + UI) | ⬜ | | Cần P4 |
| Chat MVP | Trang Chat (stream SSE qua WebHub glm-4.6, chưa tool/sandbox) | 🟨 Đang review | | Claude làm; branch `claude/project-thread-rhrt1v`; A2/A3 sẽ gắn model connection + agent vào chính trang này |

⬜ chưa bắt đầu · 🟨 đang làm/review · ✅ xong (đã merge) · ⏸ chờ gì đó

## Chi tiết P2 (đã merge #37; fix bảo mật #38)

- [x] Migration `0004_api_keys.sql`: bảng api_keys + RLS + function `api_key_verify()` SECURITY DEFINER (verify qua pgcrypto digest, touch last_used_at cùng statement, REVOKE FROM PUBLIC)
- [x] `internal/apikeys`: format `sbx_live_<keyid12>_<secret43>`, sha256 + so sánh constant-time, cache 60s (negative cache chống dội DB), `Invalidate` khi revoke
- [x] `internal/agenttoken`: EdDSA/JWT qua golang-jwt v5, `kid` = sha256(pub)[:16], TTL mặc định 15m / max 60m / min 1m, jti; verify từ chối HS256/none/kid lạ
- [x] BFF: GET/POST/DELETE `/api/orgs/{id}/api-keys` (list = member, create/revoke = admin; audit `apikey.create`/`apikey.revoke`); POST `/v1/tokens` (Bearer key → token, scope subset else 403 `scope_exceeded`, audit `agenttoken.issue`)
- [x] UI: trang "API keys" (bảng + dialog tạo với scopes checkbox/project/expiry + dialog secret-shown-once + Revoke confirm + quickstart curl có nút copy) + sidebar item
- [x] Tests Go: format/parse hostile table, scope subset, mint/verify/tamper/alg-confusion/expired, BFF flow (create→list→token→revoke→401 sau TTL cache), org isolation (404 chéo org, member không tạo được key), validation 400
- [x] Playwright: test api-keys (create → secret 1 lần → reload biến mất → revoke) — **8/8 pass local**
- [x] Bundle 141.8 KB gzip; `go test -race` + `pnpm build` xanh

Bug đã gặp (ghi để lần sau tránh):

- SELECT cột không tồn tại trong RETURNS TABLE của function SQL → lỗi runtime chỉ thấy qua test DB-gated (unit thuần không bắt được).
- Mutant "hostile" trong test parse phải chia 2 nhóm: thật sự malformed vs. well-formed-but-weird.
- **Bảo mật (Claude review #37, fix #38):** cache verify 60s trước đây key theo `key_id` công khai — sau khi key được dùng, bất kỳ secret well-formed nào cũng pass `POST /v1/tokens` trong phần giây còn lại của TTL; và một secret sai caches entry âm khoá cả key thật. **Bài học:** cache xác thực phải bind theo cả secret (sha256), KHÔNG cache lỗi DB (chỉ cache negative-lookup), map phải có bound, `Invalidate` phải xoá mọi entry của key id. Nhớ khi viết bất kỳ cache auth nào ở P3–P6.

Việc còn mở (không chặn merge):

- [ ] HSTS ở ingress k3s (annotation cert-manager) — thuộc hạ tầng, làm cùng P3
- [ ] Phân trang audit (cần cursor phía BFF) — làm cùng P3
- [ ] `ip_allowlist` đã có cột nhưng chưa enforce (enforce ở S3 middleware)

## Chi tiết P3 (S3a — đang PR)

- [x] `workspace_id` vào v1: nhận ở `POST /v1/sandboxes` (`external.workspace_id`), trả về ở read/list, filter `GET /v1/sandboxes?external_workspace_id=` (compose được với `external_user_id`/`external_project_id`/`status`; fleet list truyền query thẳng qua worker)
- [x] Cờ `require_external` cho API key (migration `0035`): tạo/list key qua `POST/GET /v1/api-keys` nhận/report cờ; key có cờ tạo sandbox thiếu `external.workspace_id` → `400 workspace_required`, chặn TRƯỚC placement (cả local lẫn worker) và trên cả legacy `POST /sandbox` (không có bypass)
- [x] Webhook lifecycle: package `internal/webhook` — `SANDBOXD_WEBHOOK_URL` + `SANDBOXD_WEBHOOK_SECRET`; event `created/started/stopped/deleted/failed`, body `{id, event, sandbox_id, external, resources, occurred_at}`, header `X-Sandboxd-Signature: sha256=<hmac(body)>` + `X-Sandboxd-Event-Id`; async, backoff luỹ thừa 1s→1h, tối đa 24h rồi drop + log
- [x] Hook points: created (handleCreate), started (v1 startAndRespond), stopped (v1 stopSandbox), deleted (handleDelete + handlePurgeSandbox), failed (abort trong handleCreate)
- [x] `docs/openapi.yaml`: workspace_id (Sandbox + CreateSandboxRequest), query param, require_external (create/list key); sửa luôn typo `enum: ealth` → `enum: [health,...]`
- [x] Tests: `internal/webhook` (sign, wire contract, retry đến khi nhận, retry window hết hạn, nil-safe); `internal/api/v1_workspace_test.go` (roundtrip tag, filter 6 case, 400 workspace_required v1+legacy, tagged → qua gate tới image policy, key thường không bị, list report cờ, emitLifecycle ký đúng); `go test ./...` xanh + `-race` sạch
- [x] (P4 làm) Portal tiêu thụ webhook: verify HMAC constant-time + chống replay (occurred_at ±5m + event id dedupe bảng `webhook_events`) — xem Chi tiết P4
- Ghi chú: webhook bắn trên node thực thi transition — fleet create bắn từ worker; cấu hình SANDBOXD_WEBHOOK_URL trên mọi node Portal cần nghe

## Chi tiết P4 (S3 + UI — đang PR #46)

- [x] Migration `0005_sandboxes.sql`: bảng `sandboxes` (pkey = sandbox id của sandboxd, RLS theo org, index org_recent + project), `idempotency_keys` (24h, prune lười khi insert), `webhook_events` (dedupe, prune 25h)
- [x] `internal/sandboxd`: client gateway — timeout mỗi call (`PORTAL_SANDBOXD_TIMEOUT` 30s), retry 1 lần CHỈ cho GET (502/503/504 + transport error; mutation không bao giờ retry — Idempotency-Key phía Portal mới là lớp retry an toàn), error envelope sandboxd giữ nguyên status/code, 5xx → 502 không lộ nội bộ; exec gửi đúng `{cmd}` tới khi A0 merge (sandboxd hiện DisallowUnknownFields)
- [x] `/v1/sandboxes*` (Bearer API key, scope map đúng bảng mục 5: create/read/exec + start/stop/delete dùng `sandbox:create`): chéo org → **404** (qua bảng sandboxes, không lộ tồn tại); `Idempotency-Key` bắt buộc hỗ trợ trên create + exec (replay cùng body → response lưu sẵn; khác body → 422 `idempotency_key_reuse`; đang chạy → 409 `idempotency_in_progress`; claim fail → giải phóng); key pin project chỉ thấy/tạo đúng project đó
- [x] Webhook receiver `POST /api/webhooks/sandboxd` (chỉ đăng ký khi có secret; KHÔNG qua withSession): HMAC-SHA256 constant-time → 401; occurred_at lệch ±5m → 400 `stale_event`; event id trùng → 200 `duplicate`; `deleted` xoá dòng index; event lạ → ghi nhận không phá; không map được workspace_id → `ignored` (2xx để sandboxd ngừng retry); apply 500 → sandboxd retry, dedupe chống double-apply
- [x] Read-through sync: GET detail + action start/stop re-read sandboxd và cập nhật index row; sandboxd 404 khi đọc → xoá row stale → 404 cho caller
- [x] BFF dashboard (`/api/orgs/{orgID}/sandboxes*`, member role, CSRF): list (`configured:false` khi chưa nối backend — trang UI hiện empty state, không lỗi), create, detail, exec (context timeout 120s, parse POSIX-ish từ 1 dòng lệnh), logs proxy (text/SSE follow), metrics stream proxy (SSE), start/stop/delete
- [x] UI: trang **Sandboxes** (bảng live poll 5s + pill màu status + preview link + cảnh báo `unreachable_nodes`; mobile 390px → card; empty state có CTA + trạng thái "backend not connected") + dialog **New sandbox** (image có gợi ý allowlist, project, resources preset, ports, egress preset, repo URL; pending + toast lỗi) + trang **detail 4 tab**: Logs (SSE follow qua EventSource, auto-scroll dừng khi cuộn lên, ring 2000 dòng), Exec (output + exit badge + timed_out/truncated; disable khi không running), Metrics (CPU/RAM + sparkline SVG thuần từ stream 2s), Network (egress + ports + preview); actions Start/Stop (pending) + Delete (ConfirmDialog nêu hậu quả); sidebar thêm "Sandboxes"
- [x] Compose dev: `--profile sandboxes` chạy sandboxd thật (auth tắt — dev only, docker.sock mount, webhook ký về portal) + build e2e base image; preview-portal khởi sandboxd trước rồi portal, Playwright thêm kịch bản create → detail → exec `echo` → delete confirm
- [x] Helm `sandboxd.{url,apiKey,webhookSecret,timeout}`; `portal/examples/agent.py` (key → token → create → exec print(1+1) → delete, stdlib only); `go test -race ./...` xanh (gateway/webhook/BFF flow, chéo-org 404, replay/idempotency 422, webhook stale/foreign/dedupe), `pnpm build` 147.9 KB gzip (< 250 KB)
- Ghi chú: quota/rate-limit/plans/ttl là P5; exec chưa có timeout trong container (A0 merge xong chỉ cần đổi client gửi thêm `timeout_s` — BFF đã chặn 120s bằng context)

## Nhật ký

| Ngày | Việc | Kết quả |
|---|---|---|
| 2026-11-11 | Kế hoạch V2 (P1–P6) viết trong PR #33 | CI xanh, đã merge |
| 2026-11-11 | P1 trên branch `portal-p1-ui` → PR #36 | Merged; preview-portal xanh sau 3 fix (storageState trong `use`, Playwright 1.65, tên duy nhất) |
| 2026-11-11 | P2 trên branch `portal-p2-api-keys` → PR #37 | Go tests + 8/8 Playwright pass local; fix bug SELECT cột không có trong RETURNS TABLE |
| 2026-11-11 | Claude review #37, fix cache verify (PR #38) | Đã merge; lỗi: cache key theo key_id công khai → giả mạo secret pass 60s; giờ bind key id + sha256(secret), không cache lỗi DB, map bounded |
| 2026-11-11 | P3/S3a trên branch `portal-p3-sandboxd-tag-webhook` | workspace_id + filter + require_external + webhook HMAC; go test ./... xanh, -race sạch; PR chờ CI |
| 2026-11-11 | P3 (PR #40) merge vào main | CI 7/7 xanh |
| 2026-11-11 | P4 trên branch `portal-p4-gateway-sandboxes` (z.ai) → PR #46 | Gateway S3 + UI Sandboxes; chi tiết ở mục "Chi tiết P4"; go test -race + pnpm build xanh |

| 2026-11-11 | Chat MVP (Claude) | Migration `0005_chat.sql` (hội thoại riêng theo user, RLS org+user), `internal/llm` (client OpenAI-compatible stream, header WebHub), BFF `/api/orgs/{id}/chats…` + SSE, trang `/orgs/:slug/chat`; env `PORTAL_DEFAULT_LLM_{BASE_URL,MODEL,FLAVOR,API_KEY}`, secret k3s `llm-api-key` từ GitHub secret `PORTAL_DEFAULT_LLM_API_KEY` |

## Ghi chú cho lần sau

- CI `portal-console` dùng `pnpm install --frozen-lockfile` — khi thêm dependency phải chạy `pnpm add` để cập nhật `pnpm-lock.yaml`, không được dùng npm.
- Playwright config: `storageState` bắt buộc nằm **trong** `use` của project (đặt ngang hàng thì bị bỏ qua im lặng — request ra không cookie). Runner GitHub hiện là ubuntu 26.04 → Playwright phải ≥ 1.65.
- Mật khẩu admin (sau P0) tối thiểu 12 ký tự; compose dev + preview lấy từ env `PORTAL_ADMIN_PASSWORD`.
- Invite URL có dạng `/invites/{orgID}?token=…` — route SPA tương ứng là `/invites/:orgId`.
- Token GitHub của agent không merge được PR (404) — owner mới merge; sau khi CI xanh chỉ cần nhắc owner.
- Chạy UI local: `cd portal/web && pnpm dev` (proxy API về `127.0.0.1:8080`, cần portal backend + postgres).
- Debug browser test local không cần Docker: Postgres nhúng (binaries zonky) + `go build ./cmd/portal` (nhớ copy `portal/web/dist/*` vào `portal/internal/webfs/dist/` trước khi build — Dockerfile làm sẵn bước này, local thì tự làm), rồi `BASE_URL=… PORTAL_ADMIN_PASSWORD=… npx playwright test`. Trace lỗi nằm ở `test-results/run/*/trace.zip`.
- Chạy browser test như CI: `docker compose -f docker-compose.portal.yml up --build -d postgres portal` rồi `cd portal/e2e && npm i && npx playwright test`.
