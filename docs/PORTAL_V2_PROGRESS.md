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
| P4 | Gateway sandboxd + trang Sandboxes (S3 + UI) | ✅ Xong | #41 + #46 (merge qua #47) | gateway /v1 + webhook + UI list/detail + preview có sandboxd thật; review fix symlink escape, pinned-key 404 chéo project, stream/exec dùng Stream client, stdin string |
| P5 | Quota + rate limit + trang Usage (S4 + UI) | ✅ Xong | #50 + #52 | Quota + rate limit + trang Usage; review #52 fix: usage membership, quota trên mọi đường run, billing vcpu_seconds, stream replay; P5b (#53) áp plan limits (vCPU/TTL/egress/monthly-vCPU) + per-bucket rate limit |
| P5b | Áp plan limits + per-bucket rate limit | 🟨 Đang review | #53 | Branch `portal-p5b-plan-limits` — CheckPlanLimits (vCPU/TTL/egress/monthly-vCPU) trong admitRun; TakeKind 5 bucket (create/exec/files/action/stream); service key docs thêm `sandboxes:files` |
| P6 | SDK + MCP + trang Quickstart (S5 + UI) | ⬜ | | Cần P4 |
| P5 | Quota + rate limit + trang Usage (S4 + UI) | 🟨 Đang review | #50 | Branch `portal-p5-quota-usage` (z.ai) — plans + usage_events + quota check trước create (advisory lock theo org) + token bucket theo key + GET /v1/usage + trang Usage UI |
| P6 | SDK + MCP + trang Quickstart (S5 + UI) | 🟨 Đang review | #55 | Branch `portal-p6-sdk-mcp-quickstart` — portal/openapi.yaml + SDK Python (17 tests) + SDK TypeScript (20 tests) + MCP server 6 tools (9 tests) + trang Quickstart + docs/portal-quickstart.md |
| Chat MVP | Trang Chat (stream SSE qua WebHub glm-4.6, chưa tool/sandbox) | ✅ Xong | #45 | Merged; A2/A3 sẽ gắn model connection + agent vào chính trang này |

### Kế hoạch AGENT (`docs/AGENT_PLAN.md`, PR #39 — A0–A7 + T1–T3)

| M | Nội dung | Trạng thái | PR | Ghi chú |
|---|---|---|---|---|
| A0 | sandboxd: exec v2 + files API (chỉ control-plane) | ✅ Xong | #41 + #46 (merge qua #47) | exec v2 (`timeout_s`/`cwd`/`env`/`stdin`/`max_output_bytes`/`?stream=ndjson`/policy `exec.max_timeout_s`) + files API 5 route + scope `sandboxes:files`; review fix: `noSymlinkComponents` chạy trước MkdirAll/chown, `chownNewParents` Lstat/Lchown + bỏ qua symlink, `execNDJSON` khóa mutex 2 goroutine |
| A1 | Portal gateway cho exec v2 + files | ✅ Xong | #51 + #52 | Gateway files + exec v2; review #52 fix: stream replay 409 idempotency_key_used, audit chỉ ghi khi sandboxd chấp nhận |
| A2 | Model connections (Portal) | ⬜ | | Cần P2 (đủ); song song A0/A1 — WebHub free mặc định `glm-4.6` (§5.2.1); spike đầu A2: tool calling khi streaming |
| A2 | Model connections (Portal) | 🟨 Phase 1 đang review | #57 | Branch `portal-a2-llm-connections` — migration 0008 + llmconfig (crypto AES-GCM + SSRF guard + resolver); phase 2 (API + Chat refactor + UI) chờ cron tiếp tục |
| A3 | Agent runtime + API | ⬜ | | Cần A0+A1+A2 |
| A4 | UI Agent + Files | ⬜ | | Cần A3 |
| A5 | Quota/usage/MCP cho agent | ⬜ | | Cần A3, P5, P6 |
| A6 | Triển khai k3s + vận hành | ⬜ | | Cần A4 |
| A7 | CLI agent trong sandbox | ⬜ | | Tuỳ chọn — để sau |
| T1 | Thư viện template | ⬜ | | Cần P4; song song A2/A3 |
| T2 | Wizard onboarding + hồ sơ khách | ⬜ | | Cần T1 |
| T3 | Agent dùng brief + skill | ⬜ | | Cần A3 + T1 |

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

- [x] Migration `0006_sandboxes.sql` (đổi số từ 0005 — `0005_chat.sql` của Chat MVP merge trước): bảng `sandboxes` (pkey = sandbox id của sandboxd, RLS theo org, index org_recent + project), `idempotency_keys` (24h, prune lười khi insert), `webhook_events` (dedupe, prune 25h)
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
| 2026-11-11 | Claude mở PR #39: `docs/AGENT_PLAN.md` (A0–A7 + T1–T3) | Docs-only, chờ merge; Q8 (ai làm A0) → z.ai nhận |
| 2026-11-11 | PR #39 cập nhật: WebHub là LLM free mặc định, model `glm-4.6` (mục 5.2.1) | Mỗi org 1 key `wh_` riêng (`key_mode=per_org`), không bao giờ dùng chung; `X-WebHub-User` = UUID (`u_<user_uuid>`, không gửi email); `agent_sessions.id` → `X-Session-Id`, `run_id:step` → `X-Request-Id`; memory tắt trong vòng lặp tool (`X-WebHub-Memory: 0`), chỉ lượt LLM đầu mỗi run bật; Portal tự ghi hồ sơ khách + `finish.summary` qua `POST /api/memory`; KHÔNG commit key nào vào repo (kể cả demo key) |
| 2026-11-11 | Câu hỏi mở còn chờ | Q14 (anh/hub trả lời): hub có API tạo/thu hồi key `wh_` để Portal tự cấp khi tạo org không? Key cấp theo org hay theo project? |
| 2026-11-11 | Spike đầu A2 (chưa kiểm chứng) | glm-4.6 qua hub có hỗ trợ `tools`/`tool_choice` và trả `tool_calls` delta khi `stream:true` không — nếu không phải có chế độ dự phòng (JSON trong text) |
| 2026-11-11 | A0 trên branch `agent-a0-exec-v2-files` (z.ai) → PR #41 | exec v2 (`timeout_s`/`cwd`/`env`/`stdin`/`max_output_bytes`/`timed_out`/`truncated`/`duration_ms`, `?stream=ndjson`, policy `exec.max_timeout_s`); files API 5 route (list/read/write/delete/move) + scope `sandboxes:files`; path safety: safeJoin + realpathWithin + noSymlinkComponents, chặn `.ssh`; go test xanh + `-race` sạch; openapi + e2e cập nhật |
| 2026-11-11 | preview.yml thêm retry 1 lần cho `playwright install --with-deps` | Preview job fail 2 lần liên tiếp vì DNS runner thoáng chốc khi apt (`Temporary failure resolving security.ubuntu.com`) — không phải code; thêm retry + fail hẳn nếu 2 lần đều lỗi |
| 2026-11-11 | A0 fix CI lần 2 (commit fix `yes A` trên PR #41) | Sau khi timeout argv hết lỗi, e2e lộ bug thứ 2 TRONG SCRIPT e2e do A0 thêm vào: payload JSON chứa `\0` (escape không hợp lệ trong JSON) → sandboxd 400 `invalid_request` đúng chuẩn. Sửa thành `yes A | head -c 50000000` (không cần escape). Bài học: chuỗi shell nhúng JSON phải kiểm tra escape hai lớp (bash + JSON); preview-portal fail lần đó chỉ là DNS runner thoáng chốc, không phải code |
| 2026-11-11 | A0 fix CI (commit `0dae9aa` trên PR #41) | lifecycle e2e fail exit 127: `timeout: failed to run command '--'` — GNU timeout ngừng parse option ở tham số vị trí đầu (duration), `--` sau duration bị coi là TÊN LỆNH. Bài học: không được đặt `--` sau duration của `timeout`; chuyển sang `timeout --kill-after=2 <n>` (TERM trước, SIGKILL 2s sau với tiến trình bỏ qua TERM, vẫn exit 124). go test ./... xanh local |
| 2026-11-11 | P4 trên branch `portal-p4-gateway-sandboxes` (z.ai) → PR #46 | Gateway S3 + UI Sandboxes; chi tiết ở mục "Chi tiết P4"; go test -race + pnpm build xanh |
| 2026-11-11 | P4 rebase lên main sau Chat MVP (#45) | Xung đột 9 file resolve giữ cả hai; migration đổi thành `0006_sandboxes.sql`; go test ./... + tsc + vite build + helm lint xanh sau rebase |

| 2026-11-11 | Chat MVP (Claude) | Migration `0005_chat.sql` (hội thoại riêng theo user, RLS org+user), `internal/llm` (client OpenAI-compatible stream, header WebHub), BFF `/api/orgs/{id}/chats…` + SSE, trang `/orgs/:slug/chat`; env `PORTAL_DEFAULT_LLM_{BASE_URL,MODEL,FLAVOR,API_KEY}`, secret k3s `llm-api-key` từ GitHub secret `PORTAL_DEFAULT_LLM_API_KEY` |

| 2026-11-12 | A0 + P4 review fix merge qua #47 | Claude review PR #41 + #46 tìm 4 lỗi; fix trên branch `claude/project-thread-ue8867` (commits `50e15ca` A0 + `b78a8d4` P4 + `9cffe88` gofmt). (1) **Symlink escape ở files API** (`PUT /files/content` + `POST /files/move`): `os.MkdirAll` và `chownNewParents` chạy TRƯỚC `noSymlinkComponents`, dùng `os.Chown` → tenant trồng `escape -> /etc` rồi `escape/newdir/f` tạo host dir + temp file ngoài workspace, `chown` luôn cả target của symlink. Fix: kiểm tra symlink trước mọi syscall follow, `chownNewParents` dùng `Lstat`/`Lchown` và bỏ qua entry là symlink. Test regression: assert không tạo gì ngoài workspace. (2) **Data race `execNDJSON`**: stdout+stderr chạy 2 goroutine, cùng `json.Encoder`/`ResponseWriter` không khóa → race + dòng đan xen. Fix: `sync.Mutex` quanh emit. (3) **Pinned-key chéo project**: route `/v1/sandboxes/{id}` chỉ check org, không check project → key pin project A exec/log/start/stop/delete sandbox project B (cùng org). Fix: `keyOwnsSandbox` yêu cầu thêm `key.ProjectID == "" || projectID == key.ProjectID`, sai → 404. (4) **Stream timeout**: `sandboxd.Client.HTTP.Timeout` = 30s áp cho mọi call, nhưng `http.Client.Timeout` tính cả thời gian đọc body → SSE logs/metrics follow và exec bị cắt ở 30s (gateway cap 120s). Fix: tách `Stream *http.Client` không Timeout, thời gian sống do `context` quyết định. (5) **stdin field**: client gửi `stdin_b64` nhưng sandboxd exec v2 (A0) nhận `stdin` chuỗi. Fix: gửi `body["stdin"] = string(in.Stdin)`. Bài học: (a) kiểm tra bảo mật đường dẫn phải đứng trước mọi syscall có follow symlink; (b) `http.Client.Timeout` tính cả thời gian đọc body, không dùng cho stream; (c) không bỏ qua check theo project khi key đã bị pin — ranh giới project cũng phải 404 như org. PR #48 (commit `106698f`): `scripts/preview/smoke.cjs` đợi nút "Open <ULID>" (regex `^Open [0-9A-HJKMNP-TV-Z]{26}$`) thay vì `^Open sbx-` — id sandbox là ULID, không có tiền tố `sbx-` |
| 2026-11-12 | P5 + A1 bắt đầu (z.ai) | Branch `portal-p5-quota-usage` và `portal-a1-gateway-files`, mỗi milestone một PR. P5: migration `0007_plans_usage.sql` (bảng `plans` + `usage_events` RLS, idempotent theo `source_event_id`), quota check trước create (đếm sandbox running trong tx khóa theo org), token bucket rate limit theo key (in-memory, fallback Postgres sau nếu cần), `GET /v1/usage` scope `usage:read`, BFF `/api/orgs/{id}/usage`, trang Usage UI (CPU/RAM/sandbox-seconds theo ngày, so với quota plan). A1: proxy 5 route `/v1/sandboxes/{id}/files*` qua `keyOwnsSandbox` + scope Portal `sandboxes:files`, dùng `sandboxd.Client` thêm method `Files*` (HTTP không retry cho mutation, stream cho content lớn); route `/v1/sandboxes/{id}/exec` nhận thêm `timeout_s/cwd/env/stdin/max_output_bytes` truyền thẳng; `?stream=ndjson` relay SSE qua `Stream` client |
| 2026-11-12 | P5 PR #50 mở + A1 PR #51 mở | P5: `portal-p5-quota-usage` → PR #50. `go test -race ./...` xanh (cần `PORTAL_TEST_DATABASE_URL`); `pnpm build` 153.23 KB gzip. Regression test `TestQuotaExceededReturns429` fail trên code cũ (trả 201) và pass trên code mới (429 quota_exceeded). Quota dùng `pg_advisory_xact_lock(hashtext(org_id))` để serialize concurrent creates của cùng org. A1: `portal-a1-gateway-files` → PR #51. `TestFilesGatewayCrossOrgReturns404` kiểm tra key org B không đọc/ghi/xóa/move file sandbox org A → 404 (không lộ tồn tại). PUT body forward verbatim (không JSON-wrap). Exec v2 fields (timeout_s/cwd/env/stdin/max_output_bytes) truyền thẳng; `?stream=ndjson` relay qua `sandboxd.Client.ExecStream` (Stream client). Hai PR độc lập, merge thứ tự nào cũng được |
| 2026-11-12 | P5 + A1 merge qua PR #52 (kèm review fix) | Claude review #50 + #51 tìm 5 lỗi; fix trên branch `claude/project-thread-ue8867` (commit `89edac5`). (1) **Usage membership**: `GET /api/orgs/{id}/usage` set `app.org_id` từ path mà không check membership → mọi user đọc usage mọi org. Fix: require membership (404 nếu không). (2) **Quota bypass**: chỉ guard `POST /v1/sandboxes`, dashboard create + start (v1 + dashboard) bypass; advisory lock release trước create → concurrent creates đều pass. Fix: `admitRun` gate mọi đường run, giữ per-org lock qua check + create + index insert. (3) **Billing vcpu_seconds**: chưa bao giờ ghi (monthly quota stuck 0); run đầu sau create không billed (started_at chỉ set trên 'started'); stop rồi delete billed 2 lần. Fix: window mở trên create/start, đóng 1 lần trên stop/delete đầu tiên; sandbox_seconds + vcpu_seconds ghi cùng tx với index update. (4) **Webhook apply fail** không drop dedupe row → sandboxd retry bị ack là duplicate. Fix: drop dedupe row khi apply fail. (5) **A1 stream replay**: `?stream=ndjson` với reused Idempotency-Key re-ran command. Fix: replay trả 409 `idempotency_key_used`. (6) **A1 audit**: file write/delete/move audit ngay cả khi sandboxd refuse. Fix: audit chỉ khi sandboxd chấp nhận. Mỗi fix có test fail trên code cũ |
| 2026-11-12 | P5b PR #53 mở: plan limits + per-bucket rate limit | Review #52 phát hiện plan limits (vCPU/TTL/egress/monthly-vCPU) mới chỉ lưu, chưa áp dụng. Branch `portal-p5b-plan-limits` → PR #53. (1) `quota.CheckPlanLimits` chạy trong RLS context, trả `*LimitViolation{Field, Limit, Used}` → 422 `plan_limit` envelope. (2) `admitRun` nhận `quota.CreateRequest`, gọi CheckPlanLimits sau CheckConcurrentSandboxes (van duoi per-org mutex). (3) `createSandboxBody` + `sandboxd.CreateInput` thêm `TTLSeconds` → gửi như `lifecycle.idle_timeout_secs`. (4) `ratelimit.TakeKind(keyID, kind, rps, cost)` compose bucket key `<kind>:<keyID>`, 5 bucket: create/exec/files/action/stream. (5) `Server.rateLimit` helper load plan, charge 1 token, 429 + Retry-After. Wired: create, exec, 5 files routes, start/stop/delete, logs. (6) Helm values + README: service key phải có `sandboxes:files`. Tests: 4 plan limits, race 2 concurrent, per-bucket rate limit, e2e files round-trip qua Portal. `go test -race` xanh, gofmt sạch |

## Ghi chú cho lần sau

- CI `portal-console` dùng `pnpm install --frozen-lockfile` — khi thêm dependency phải chạy `pnpm add` để cập nhật `pnpm-lock.yaml`, không được dùng npm.
- Playwright config: `storageState` bắt buộc nằm **trong** `use` của project (đặt ngang hàng thì bị bỏ qua im lặng — request ra không cookie). Runner GitHub hiện là ubuntu 26.04 → Playwright phải ≥ 1.65.
- Mật khẩu admin (sau P0) tối thiểu 12 ký tự; compose dev + preview lấy từ env `PORTAL_ADMIN_PASSWORD`.
- Invite URL có dạng `/invites/{orgID}?token=…` — route SPA tương ứng là `/invites/:orgId`.
- Token GitHub của agent không merge được PR (404) — owner mới merge; sau khi CI xanh chỉ cần nhắc owner.
- Chạy UI local: `cd portal/web && pnpm dev` (proxy API về `127.0.0.1:8080`, cần portal backend + postgres).
- Debug browser test local không cần Docker: Postgres nhúng (binaries zonky) + `go build ./cmd/portal` (nhớ copy `portal/web/dist/*` vào `portal/internal/webfs/dist/` trước khi build — Dockerfile làm sẵn bước này, local thì tự làm), rồi `BASE_URL=… PORTAL_ADMIN_PASSWORD=… npx playwright test`. Trace lỗi nằm ở `test-results/run/*/trace.zip`.
- Chạy browser test như CI: `docker compose -f docker-compose.portal.yml up --build -d postgres portal` rồi `cd portal/e2e && npm i && npx playwright test`.
- **Bài học review #47 (bắt buộc nhớ khi viết P5/A1 trở đi):**
  - Kiểm tra bảo mật đường dẫn (symlink component, `..`, escape) phải chạy TRƯỚC mọi syscall có follow symlink (`MkdirAll`, `CreateTemp`, `Chown`, `Stat`…). `Lstat` + `Lchown` cho mọi chown đi qua path do tenant kiểm soát, bỏ qua entry là symlink.
  - `http.Client.Timeout` tính cả thời gian đọc body — không dùng cho stream (SSE follow, exec dài). Tách `Stream *http.Client` không Timeout, thời gian sống do `context` của caller quyết định.
  - Khi API key bị pin vào một project (`key.ProjectID != ""`), quyền sở hữu phải check cả org LẪN project; sai project trả 404 (không lộ tồn tại), giống chéo org.
  - Hai goroutine dùng chung `json.Encoder`/`http.ResponseWriter` phải có `sync.Mutex` quanh emit, nếu không có data race và dòng đan xen.
  - Field contract giữa Portal client và sandboxd phải khớp tên + kiểu: A0 exec v2 nhận `stdin` (chuỗi), không phải `stdin_b64`.
- **Preview smoke (`scripts/preview/smoke.cjs`):** id sandbox là ULID 26 ký tự, không có tiền tố `sbx-`. Regex đợi nút "Open <id>" phải là `^Open [0-9A-HJKMNP-TV-Z]{26}$`. Nếu preview đỏ, đọc log xem bước nào fail trước khi kết luận DNS flake; chỉ tính flake khi log có `Temporary failure resolving` ở bước cài deps.
- **P6 PR #55 mở (z.ai):** `portal-p6-sdk-mcp-quickstart`. `portal/openapi.yaml` (mới) là nguồn sự thật cho /v1 API. SDK Python (17 tests, stdlib only) + SDK TypeScript (20 tests, Node stdlib only) viết tay khop openapi; auto Idempotency-Key cho create/exec; `exec_stream` (NDJSON) + `logs_follow` (SSE) generators; `write_file` gửi raw bytes (octet-stream), KHÔNG JSON-wrap. MCP server (`portal/internal/mcp/` + `portal/cmd/mcp-server/`) Streamable HTTP transport, 6 tools (sandbox_create/exec/delete + read_file/write_file/list_files), forward Bearer token của caller; 9 tests. Trang Quickstart (`/orgs/:slug/quickstart`) 3 tab Python/TS/curl + CopyButton + MCP JSON config; Sandboxes empty state link tới Quickstart. `docs/portal-quickstart.md` companion long-form. `go test -race` xanh, `pnpm build` 154.91 KB gzip. Bài học: SDK phải khớp openapi từng field (`stdin` chuỗi, `write_file` raw bytes, 422 `plan_limit` có `field`/`limit`/`used`); MCP server `write_file` build PUT manually vì body là octet-stream, không qua `portalCall` JSON.
- **A2 phase 1 PR #57 mở (z.ai):** Branch `portal-a2-llm-connections`. Migration `0008_model_connections.sql`: bảng `model_connections` (org_id NULL = platform, RLS cho org) + `model_connection_keys` (per-org, AES-GCM encrypted). Package `portal/internal/llmconfig`: `ResolveEffective` (org default → platform default → ErrNotFound), `MasterKey` (AES-256 từ PORTAL_SECRETS_KEY: raw 32-byte base64/hex hoặc passphrase Argon2id), `Encrypt/Decrypt` (AES-256-GCM), `IsPrivateBaseURL` SSRF guard (127/8, 10/8, 172.16, 192.168, 169.254, ::1, fc00, fe80, DNS fail). Tests: round-trip, tampered ciphertext fails (GCM auth tag), passphrase deterministic, missing env errors, 13 SSRF cases. Phase 2 (chưa làm, cron tiếp): API BFF + /v1, POST /test, GET /models, Chat MVP refactor, WebHub header, UI, e2e tests.
