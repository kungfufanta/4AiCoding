# PORTAL — TIẾN ĐỘ GIAI ĐOẠN 2 (P1–P6)

File theo dõi cho kế hoạch [`docs/PORTAL_PLAN_V2.md`](PORTAL_PLAN_V2.md). Cập nhật **mỗi PR**: đổi cột Trạng thái, thêm một dòng vào Nhật ký. Đọc file này trước khi bắt đầu milestone kế.

## Trạng thái milestone

| M | Nội dung | Trạng thái | PR | Ghi chú |
|---|---|---|---|---|
| P0 | Xiết chặt đăng nhập (bỏ mật khẩu mặc định) | ✅ Xong | #34 | Claude khác làm (main `1bda85c`) |
| Kế hoạch V2 | `docs/PORTAL_PLAN_V2.md` | ✅ Xong | #33 | Docs-only, CI xanh |
| **P1** | Design system + app shell + migrate Login/Projects/Members/Audit + trang accept-invite + CSP + preview workflow | 🟨 Đang review | #36 | Branch `portal-p1-ui` |
| P2 | API key + agent token (S2 + UI) | ⬜ | | Cần P1 merge |
| P3 | Tag tenant + webhook ở sandboxd (S3a) | ⬜ | | Độc lập, có thể song song với P2 |
| P4 | Gateway sandboxd + trang Sandboxes (S3 + UI) | ⬜ | | Cần P2 + P3 |
| P5 | Quota + rate limit + trang Usage (S4 + UI) | ⬜ | | Cần P4 |
| P6 | SDK + MCP + trang Quickstart (S5 + UI) | ⬜ | | Cần P4 |

⬜ chưa bắt đầu · 🟨 đang làm/review · ✅ xong (đã merge) · ⏸ chờ gì đó

## Chi tiết P1 (PR đang mở)

- [x] Tailwind CSS 4 + token thiết kế (`@theme inline`, dark/light qua class + `prefers-color-scheme`)
- [x] Bộ component `ui/`: button, input/field, select (Radix), dropdown (Radix), dialog (Radix) + ConfirmProvider, toast, table, badge, card, skeleton/empty-state, copy-field
- [x] react-router (v8, library mode): `/login`, `/invites/:orgId`, `/orgs/:orgSlug/{projects,members,audit}` — webfs đã có SPA fallback
- [x] TanStack Query 5; không còn `prompt()`/`alert()`; destructive → ConfirmDialog
- [x] AppShell: sidebar (drawer < lg) + topbar (org switcher, user menu, theme toggle)
- [x] Trang accept-invite (vá thiếu sót S1: API có, UI không có) — route `/invites/{orgID}?token=…`
- [x] SSO deep-link: `GET /auth/login?next=…` (sanitize same-origin, test `TestSanitizeNext`)
- [x] CSP: `style-src 'self' 'unsafe-inline'` (Radix đặt style attribute; script-src giữ `'self'`)
- [x] Bundle 139.8 KB gzip (< mục tiêu 250 KB); `pnpm build` xanh; `go test -race ./...` xanh
- [x] Playwright: `portal/e2e` (login + projects + invite + audit + dark + mobile 390px, chụp 9 ảnh) + workflow `preview-portal.yml` (paths-filter portal/**)

Việc còn mở (không chặn merge):

- [ ] HSTS ở ingress k3s (annotation cert-manager) — thuộc hạ tầng, làm cùng P2
- [ ] Phân trang audit (cần cursor phía BFF) — làm cùng P2 backend

## Nhật ký

| Ngày | Việc | Kết quả |
|---|---|---|
| 2026-11-11 | Kế hoạch V2 (P1–P6) viết trong PR #33 | CI xanh, chờ duyệt |
| 2026-11-11 | P1 làm trên branch `portal-p1-ui` → PR #36 | `pnpm build` + `go test -race` xanh local |
| 2026-11-11 | preview-portal fail 2 lần → tái hiện local (Postgres nhúng + portal binary) | 3 fix: `storageState` phải nằm trong `use` của project Playwright; Playwright ≥ 1.65 cho runner ubuntu 26.04; tên project/email dùng `Date.now()` tránh 409 khi rerun. **7/7 test pass local** |

## Ghi chú cho lần sau

- CI `portal-console` dùng `pnpm install --frozen-lockfile` — khi thêm dependency phải chạy `pnpm add` để cập nhật `pnpm-lock.yaml`, không được dùng npm.
- Playwright config: `storageState` bắt buộc nằm **trong** `use` của project (đặt ngang hàng thì bị bỏ qua im lặng — request ra không cookie). Runner GitHub hiện là ubuntu 26.04 → Playwright phải ≥ 1.65.
- Mật khẩu admin (sau P0) tối thiểu 12 ký tự; compose dev + preview lấy từ env `PORTAL_ADMIN_PASSWORD`.
- Invite URL có dạng `/invites/{orgID}?token=…` — route SPA tương ứng là `/invites/:orgId`.
- Token GitHub của agent không merge được PR (404) — owner mới merge; sau khi CI xanh chỉ cần nhắc owner.
- Chạy UI local: `cd portal/web && pnpm dev` (proxy API về `127.0.0.1:8080`, cần portal backend + postgres).
- Debug browser test local không cần Docker: Postgres nhúng (binaries zonky) + `go build ./cmd/portal` (nhớ copy `portal/web/dist/*` vào `portal/internal/webfs/dist/` trước khi build — Dockerfile làm sẵn bước này, local thì tự làm), rồi `BASE_URL=… PORTAL_ADMIN_PASSWORD=… npx playwright test`. Trace lỗi nằm ở `test-results/run/*/trace.zip`.
- Chạy browser test như CI: `docker compose -f docker-compose.portal.yml up --build -d postgres portal` rồi `cd portal/e2e && npm i && npx playwright test`.
