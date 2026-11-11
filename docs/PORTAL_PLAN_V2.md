# KẾ HOẠCH GIAI ĐOẠN 2 — Portal: UI hiện đại + hoàn thiện chức năng

Tài liệu này **bổ sung** cho `docs/PORTAL_PLAN.md` (vai trò hợp đồng kiến trúc, mục 0–5 giữ nguyên). Mục 6 của PORTAL_PLAN.md được lập lại lịch ở đây thành **P1–P6**: nội dung S2–S5 giữ nguyên, chỉ gói lại cùng phần UI để mỗi PR giao được một sản phẩm nhìn thấy được.

Yêu cầu gốc (2026-11-11): *"Portal đã lên khung giao diện, nhưng chức năng và giao diện còn khá tối cổ, cần kế hoạch mới."*

Mỗi milestone là **một PR** vào `main`, có tiêu chí "Xong khi" và test chứng minh. Sau mỗi milestone có checkpoint: báo kết quả, chờ duyệt rồi mới sang milestone kế (PLAN.md §9).

## 0. Bối cảnh và hiện trạng (audit 2026-11-11)

### 0.1 Đã xong

| Việc | Bằng chứng |
|---|---|
| S0 — khung dự án (Go module, Vite + React 19, migrations, chart, CI) | commit `39b42bb` |
| S1 — OIDC BFF, web sessions, CSRF, RLS multi-tenancy | commit `cf3ae57` |
| Password bootstrap admin (`admin/admin123`, migration `0003`) | commit `8328d30` |
| CI/CD k3s, domain `chat.devhub.io.vn` | PR #17 |
| Zitadel tự host + script cấu hình OIDC tự động | PR #29–#32 (`scripts/zitadel_configure.py`) |
| SPA fallback + security headers (CSP, X-Frame-Options, nosniff) ở `webfs` | có sẵn |

### 0.2 Đang thiếu

**Giao diện** — toàn bộ dashboard là một file `App.tsx` 323 dòng + 159 dòng CSS:

- Không router, không điều hướng (tab state trong memory, refresh mất vị trí).
- Nhập liệu bằng `prompt()`, báo lỗi bằng `alert()` — không validation, không xử lý lỗi nhất quán.
- Không dark mode, không responsive (vỡ ở mobile), không empty state, không skeleton/spinner có chủ đích.
- Bảng Members/Audit là `<ul>` thô; không bảng, không lọc, không phân trang.
- **Thiếu cả trang nhận lời mời**: API `POST /api/orgs/{id}/invites/accept` đã có, UI chưa có — invite link gửi cho đồng nghiệp hiện không có chỗ để đi tới.

**Chức năng** — luồng giá trị "tạo API key → tạo sandbox → xem log/usage" chưa có bước nào trong dashboard:

- S2 (API key + agent token), S3a (thay đổi sandboxd), S3 (gateway), S4 (quota/usage), S5 (SDK/MCP): chưa bắt đầu.
- Dashboard API BFF hiện chỉ có 10 endpoint (me/orgs/projects/members/invites/audit) — đúng phạm vi S1, không rộng hơn.

## 1. Mục tiêu giai đoạn 2

1. **UI đạt chuẩn SaaS hiện đại** (đối chiếu: Linear, Vercel, Stripe dashboard): sidebar + topbar, dark/light, responsive, dialog/toast/table đúng chuẩn, accessibility cơ bản.
2. **Thông suốt luồng giá trị** trong dashboard: login → org/project → tạo API key → tạo sandbox → xem log/exec → usage.
3. **Không phá hợp đồng**: nguyên tắc bất biến (PORTAL_PLAN mục 1), checklist bảo mật (mục 7), hợp đồng BFF API hiện có (e2e Go cũ phải vẫn xanh).

## 2. Quyết định nền tảng (UI)

Các mục đánh dấu *(giả định)* là mặc định khi chưa có quyết định khác của chủ dự án.

| Vấn đề | Quyết định | Lý do |
|---|---|---|
| Styling | **Tailwind CSS 4**, token qua `@theme` (màu/spacing/radius), dark mode bằng class + `prefers-color-scheme` | CSS build-time tĩnh, không cần runtime CSS-in-JS |
| Component | **Tự viết (copy-in, kiểu shadcn) trên Radix UI primitives**: Dialog, DropdownMenu, Select, Popover, Tooltip, Toast | Accessible sẵn (focus trap, aria), code nằm trong repo, không phụ thuộc bộ UI đóng gói |
| Icons | `lucide-react` | Gọn, đồng bộ style |
| Router | **react-router v7 (library mode)**; `webfs` đã có SPA fallback nên không đổi Go | Refresh giữ được URL |
| Data layer | **TanStack Query 5** (polling, invalidate sau mutation); log stream SSE giữ EventSource/fetch thuần | Không tự viết cache/polling tay |
| Charts | SVG tự vẽ (bar/area đơn giản) | Không thêm thư viện chart ở giai đoạn này |
| Exec trong UI | Form nhập lệnh + khối output; `xterm.js` chỉ nâng cấp khi có nhu cầu thật | Đủ dùng cho debug, bundle nhỏ |
| CSP | Mở `style-src 'self' 'unsafe-inline'` trong `webfs`; `script-src` giữ `'self'` nghiêm ngặt | Radix đặt style attribute để định vị popover/dialog; CSP hiện tại sẽ chặn gãy UI. Cập nhật checklist §7 của PORTAL_PLAN theo đó |
| i18n | Tiếng Anh, không i18n giai đoạn này | Nhất quán với hiện trạng |
| Bundle | Mục tiêu < 250 KB gzip sau P1, in size ra log CI ở mỗi PR UI | Chống phình |

## 3. Kiến trúc UI

```
portal/web/src/
├── app/                    # router, providers (QueryClient, Theme, Toast), ErrorBoundary
├── components/
│   ├── ui/                 # button, input, select, badge, card, dialog, dropdown,
│   │                       # toast, table, tabs, skeleton, empty-state, code-block
│   └── layout/             # AppShell, Sidebar, Topbar, OrgSwitcher, UserMenu
├── features/
│   ├── auth/               # LoginPage (SSO + password)
│   ├── invites/            # AcceptInvitePage (P1)
│   ├── projects/           # P1
│   ├── members/            # P1
│   ├── audit/              # P1
│   ├── api-keys/           # P2
│   ├── sandboxes/          # P4 (list, detail: logs/exec/metrics/network)
│   ├── usage/              # P5
│   └── quickstart/         # P6
└── lib/                    # api client (giữ hợp đồng cũ), hooks, format helpers
```

Nguyên tắc:

- `api.ts` giữ nguyên hình thái (ApiError + CSRF header); thêm endpoint mới theo từng PR, không viết lớp client "phòng khi".
- Mỗi feature tự chứa data hooks của nó (query key, mutation, invalidate). Không có store toàn cục ngoài QueryClient + org context (org đang chọn lưu ở URL, không lưu state).
- Route có org: `/orgs/:orgSlug/...`; OrgSwitcher đổi slug → navigate. URL là nguồn sự thật của ngữ cảnh.

## 4. Checklist chất lượng UI (bắt buộc cho mọi PR có UI)

- [ ] Không còn `prompt()`/`alert()`/`confirm()` native; lỗi → Toast, phá hủy → ConfirmDialog nêu rõ hậu quả.
- [ ] Mọi mutation có trạng thái pending (nút spinner + disable), thành công có phản hồi (toast hoặc cập nhật danh sách).
- [ ] Loading có skeleton; danh sách/bảng có empty state kèm CTA.
- [ ] Responsive: 390 px không vỡ layout (sidebar → drawer, bảng → thẻ xếp dọc).
- [ ] Focus visible; nút icon-only có `aria-label`; dialog có focus trap (Radix lo).
- [ ] Secret/API key hiển thị đúng một lần trong dialog có nút copy; không bao giờ nằm lại trong state sau khi đóng.
- [ ] `pnpm build` xanh; size bundle in ra log CI.

## 5. Milestones

### P1 — Design system + app shell + dọn dẹp S1 (1 PR)

- Thêm Tailwind 4 (`@theme` tokens: màu brand, semantic success/warning/danger, dark mode), bộ component `components/ui` (mục 3), `lucide-react`, react-router v7, TanStack Query, Toast + ConfirmDialog provider, ErrorBoundary.
- AppShell: sidebar trái (Projects, Members, API keys, Audit — các mục mới thêm dần theo PR sau), collapse thành drawer < 768 px; topbar: OrgSwitcher (dropdown), user menu (email, Sign out), theme toggle.
- Migrate 4 màn hình: **Login** (SSO + password, giữ đúng `/auth/methods` quyết định hiển thị), **Projects** (tạo project qua Dialog, không prompt), **Members** (bảng, đổi role qua dropdown có confirm, mời thành viên qua Dialog, **thêm AcceptInvitePage** cho route `/invite?token=...` — vá thiếu sót S1), **Audit** (bảng, filter theo action, "load more").
- Mở CSP `style-src 'self' 'unsafe-inline'` ở `webfs` + test header không gãy.
- **Xong khi:** `pnpm build` xanh; không còn `prompt()`/`alert()` trong `portal/web`; e2e Go hiện có vẫn xanh (hợp đồng BFF không đổi); workflow preview portal mới chạy Playwright trên compose dev chụp: login, projects, members, audit, accept-invite, dark mode, mobile 390 px; luồng invite end-to-end (mời → mở link → vào org) chạy được trên compose dev.

### P2 — API key + agent token: S2 + UI (1 PR)

Backend đúng S2 gốc (PORTAL_PLAN 6/S2): key `sbx_live_<keyid>_<secret>`, lưu `sha256`, scopes, TTL, thu hồi bị từ chối ≤ 60 s, `POST /v1/tokens` EdDSA có `kid`. Thêm BFF endpoints: tạo/liệt kê/thu hồi key (`/api/orgs/{orgID}/api-keys`).

UI: trang **API keys** — nút "Create key" (Dialog: name, scopes theo nhóm, project, expiry tuỳ chọn) → Dialog "Key created" hiển thị secret đúng một lần với nút copy và cảnh báo; bảng key (name, scope badges, project, last_used_at, expires_at, Revoke có confirm); khối CodeBlock "Get started" (curl đổi token + gọi sandbox) có nút copy.

- **Xong khi:** tiêu chí S2 gốc; test UI e2e: tạo key → secret hiện một lần → reload không còn → revoke → key chết ≤ 60 s; secret không bao giờ xuất hiện trong log server (test grep log).

### P3 — Tag tenant + webhook ở sandboxd: S3a (1 PR, chạy song song được với P2)

Đúng mục 4 của PORTAL_PLAN: `workspace_id` vào v1 create/read, filter `GET /v1/sandboxes?external_workspace_id=`, cờ bắt buộc tag cho key của Portal, webhook lifecycle ký HMAC. Cập nhật `docs/openapi.yaml`.

- **Xong khi:** như S3a gốc; CI hiện có của control-plane vẫn xanh.

### P4 — Gateway sandboxd + trang Sandboxes: S3 + UI (1 PR, cần P2 + P3)

Backend đúng S3 gốc (client sandboxd có timeout/retry, kiểm tra sở hữu → 404, idempotency, nhận webhook HMAC cập nhật trạng thái `sandboxes`). BFF dashboard endpoints cho danh sách/chi tiết/tác vụ sandbox.

UI trang **Sandboxes**: bảng (status pill màu theo trạng thái, project, vCPU/RAM, age, auto-refresh 5 s bằng TanStack Query), nút "New sandbox" (Dialog/Drawer form ánh xạ `sandboxspec`: image trong allowlist, resources, ports, egress preset, repo URL, ttl); trang chi tiết: tab **Logs** (SSE stream, auto-scroll + pause khi cuộn lên), tab **Exec** (input lệnh + khối output + exit code), tab **Metrics** (CPU/RAM), tab **Network** (egress allowlist, ports), Actions start/stop/delete với ConfirmDialog.

- **Xong khi:** tiêu chí S3 gốc (script `portal/examples/agent.py` chạy trên dev; exec chéo org → 404; idempotency không tạo sandbox thứ hai) + Playwright: tạo sandbox từ UI trên compose dev, thấy status chuyển, exec ra output, delete có confirm.

### P5 — Quota + rate limit + trang Usage: S4 + UI (1 PR, cần P4)

Backend đúng S4 gốc (`plans`, quota đếm trong transaction có khoá org, rate limit token bucket theo key, `usage_events` từ webhook, `GET /v1/usage`).

UI trang **Usage**: thẻ quota còn lại (concurrent sandboxes, vCPU-seconds tháng này) với thanh tiến độ; biểu đồ bar/area SVG theo ngày; bảng usage events gần đây; banner cảnh báo khi ≥ 80 % quota; lỗi 429 `quota_exceeded` ở Sandboxes hiển thịToast có nội dung rõ.

- **Xong khi:** tiêu chí S4 gốc (429 quota_exceeded; race 50 request không vượt quota; usage ±1 phút); UI hiển thị đúng số liệu của test.

### P6 — SDK + MCP + trang Quickstart: S5 + UI (1 PR, cần P4)

Backend đúng S5 gốc (`portal/openapi.yaml` là nguồn sự thật, SDK Python/TS mỏng, MCP server Streamable HTTP, `docs/portal-quickstart.md`).

UI trang **Quickstart** ("Get started"): tabs Python / TypeScript / curl với CodeBlock copy; khối JSON config MCP server; empty state của trang Sandboxes dẫn tới Quickstart.

- **Xong khi:** tiêu chí S5 gốc (agent chạy code qua MCP không viết HTTP tay; SDK test với server giả); snippet trong Quickstart trích từ SDK thật, không viết tay.

## 6. Thứ tự và phụ thuộc

```
P1 ──▶ P2 ─┐
   └─▶ P3 ─┴─▶ P4 ──▶ P5 ──▶ P6
```

- P2 và P3 độc lập, chạy song song sau P1 (P3 chỉ đụng `control-plane/`).
- Mỗi milestone một PR; CI xanh mới chuyển milestone kế; checkpoint chờ duyệt theo PLAN.md §9.

## 7. Ngoài phạm vi giai đoạn 2

- S6 billing (giữ nguyên điều kiện "chỉ khi mở cho khách ngoài").
- Data plane trực tiếp S3b (chỉ khi đo thấy proxy `exec` là nút thắt).
- i18n, SSR, native app, terminal `xterm.js` đầy đủ, theme logo Zitadel (cấu hình trên console Zitadel, không phải code).

## 8. Rủi ro và ghi chú

| Rủi ro | Ứng phó |
|---|---|
| CSP `style-src 'unsafe-inline'` nới lỏng hơn hiện trạng | Chỉ mở cho style; `script-src 'self'` giữ nghiêm ngặt; ghi chú vào checklist §7 PORTAL_PLAN |
| Bundle phình do Tailwind + Radix + router + query | Mục tiêu < 250 KB gzip, in size ra log CI mỗi PR UI; Radix import từng primitive |
| UI mới break hợp đồng BFF hiện có | e2e Go của S1 chạy lại trong mọi PR portal; `api.ts` chỉ thêm, không đổi hình thái |
| Thay đổi sandboxd (P3) làm gãy console admin | P3 giữ nguyên hành vi mặc định (tag tuỳ chọn, chỉ key có cờ mới bắt buộc) |
| Zitadel đã lên nhưng theme/logo chưa chỉnh | Việc cấu hình trên console Zitadel, không chặn P1; SSO đã hoạt động qua script cấu hình |
