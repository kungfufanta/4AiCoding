# PORTAL — TIẾN ĐỘ GIAI ĐOẠN 2 (P1–P6)

File theo dõi cho kế hoạch [`docs/PORTAL_PLAN_V2.md`](PORTAL_PLAN_V2.md). Cập nhật **mỗi PR**: đổi cột Trạng thái, thêm một dòng vào Nhật ký. Đọc file này trước khi bắt đầu milestone kế.

## Trạng thái milestone

| M | Nội dung | Trạng thái | PR | Ghi chú |
|---|---|---|---|---|
| P0 | Xiết chặt đăng nhập (bỏ mật khẩu mặc định) | ✅ Xong | #34 | Claude khác làm (main `1bda85c`) |
| Kế hoạch V2 | `docs/PORTAL_PLAN_V2.md` | ✅ Xong | #33 | Docs-only, CI xanh |
| P1 | Design system + app shell + migrate Login/Projects/Members/Audit + trang accept-invite + CSP + preview workflow | ✅ Xong | #36 | Merged 07:27 |
| **P2** | API key + agent token (S2 + UI) | 🟨 Đang review | #37 | Branch `portal-p2-api-keys` |
| P3 | Tag tenant + webhook ở sandboxd (S3a) | ⬜ | | Độc lập — làm tiếp |
| P4 | Gateway sandboxd + trang Sandboxes (S3 + UI) | ⬜ | | Cần P2 + P3 |
| P5 | Quota + rate limit + trang Usage (S4 + UI) | ⬜ | | Cần P4 |
| P6 | SDK + MCP + trang Quickstart (S5 + UI) | ⬜ | | Cần P4 |

⬜ chưa bắt đầu · 🟨 đang làm/review · ✅ xong (đã merge) · ⏸ chờ gì đó

## Chi tiết P2 (PR đang mở)

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

Việc còn mở (không chặn merge):

- [ ] HSTS ở ingress k3s (annotation cert-manager) — thuộc hạ tầng, làm cùng P3
- [ ] Phân trang audit (cần cursor phía BFF) — làm cùng P3
- [ ] `ip_allowlist` đã có cột nhưng chưa enforce (enforce ở S3 middleware)

## Nhật ký

| Ngày | Việc | Kết quả |
|---|---|---|
| 2026-11-11 | Kế hoạch V2 (P1–P6) viết trong PR #33 | CI xanh, đã merge |
| 2026-11-11 | P1 trên branch `portal-p1-ui` → PR #36 | Merged; preview-portal xanh sau 3 fix (storageState trong `use`, Playwright 1.65, tên duy nhất) |
| 2026-11-11 | P2 trên branch `portal-p2-api-keys` → PR #37 | Go tests + 8/8 Playwright pass local; fix bug SELECT cột không có trong RETURNS TABLE |

## Ghi chú cho lần sau

- CI `portal-console` dùng `pnpm install --frozen-lockfile` — khi thêm dependency phải chạy `pnpm add` để cập nhật `pnpm-lock.yaml`, không được dùng npm.
- Playwright config: `storageState` bắt buộc nằm **trong** `use` của project (đặt ngang hàng thì bị bỏ qua im lặng — request ra không cookie). Runner GitHub hiện là ubuntu 26.04 → Playwright phải ≥ 1.65.
- Mật khẩu admin (sau P0) tối thiểu 12 ký tự; compose dev + preview lấy từ env `PORTAL_ADMIN_PASSWORD`.
- Invite URL có dạng `/invites/{orgID}?token=…` — route SPA tương ứng là `/invites/:orgId`.
- Token GitHub của agent không merge được PR (404) — owner mới merge; sau khi CI xanh chỉ cần nhắc owner.
- Chạy UI local: `cd portal/web && pnpm dev` (proxy API về `127.0.0.1:8080`, cần portal backend + postgres).
- Debug browser test local không cần Docker: Postgres nhúng (binaries zonky) + `go build ./cmd/portal` (nhớ copy `portal/web/dist/*` vào `portal/internal/webfs/dist/` trước khi build — Dockerfile làm sẵn bước này, local thì tự làm), rồi `BASE_URL=… PORTAL_ADMIN_PASSWORD=… npx playwright test`. Trace lỗi nằm ở `test-results/run/*/trace.zip`.
- Chạy browser test như CI: `docker compose -f docker-compose.portal.yml up --build -d postgres portal` rồi `cd portal/e2e && npm i && npx playwright test`.
