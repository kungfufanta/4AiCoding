# KẾ HOẠCH — Portal (lớp SaaS cho multi-agent gọi sandboxd)

Tài liệu này là hợp đồng cho người (hoặc AI) triển khai Portal. Đọc hết mục 0–4 trước khi viết code. Mỗi milestone ở mục 6 là **một PR**, có tiêu chí "Xong khi" phải đạt và có test chứng minh.

Bản thảo thảo luận gốc: https://claude.ai/artifact/RH8GQZC7yofz5pZUDbs3rD (2026-11-10, đã được duyệt).

## 0. Mục tiêu

Hệ thống multi-agent (orchestrator + nhiều sub-agent) cần gọi API để tạo sandbox, chạy lệnh, đọc kết quả rồi huỷ sandbox. sandboxd (`control-plane/`) đã làm được việc đó, nhưng nó là **admin orchestrator một tenant**: user nội bộ, API key cấp instance, console admin.

Portal là lớp đứng trước sandboxd, lo mọi thứ "thuộc về khách hàng":

- đăng nhập người dùng, tổ chức (org), project, thành viên;
- API key và token ngắn hạn cho agent;
- quota, rate limit, usage, (sau này) billing;
- API công khai ổn định có version, SDK, MCP server.

sandboxd giữ mọi thứ "thuộc về máy": chạy sandbox, policy, approvals, egress, worker fleet, audit hash chain.

## 1. Nguyên tắc bất biến

1. **sandboxd không lộ ra Internet cho khách.** Chỉ Portal gọi control plane của sandboxd, qua mạng nội bộ. Console admin của sandboxd chỉ cho operator (VPN hoặc IP allowlist).
2. **Agent không bao giờ cầm credential sandboxd dài hạn.** Agent dùng credential của Portal. Credential sandboxd duy nhất mà agent có thể thấy là key TTL ngắn gắn một `sandbox_id` (mục 3.3).
3. **Cô lập tenant ở hai lớp.** Portal kiểm tra quyền sở hữu trước mọi lệnh gọi; sandboxd kiểm tra lại bằng tag tenant và key gắn sandbox. Một bug ở Portal không được phép cho org A chạm vào sandbox của org B.
4. **Không tự viết password/MFA.** Người dùng đăng nhập qua OIDC.
5. **Secret chỉ lưu hash.** API key hiển thị đúng một lần khi tạo.
6. **Mọi thứ chạy trong sandbox là không đáng tin**, giống PLAN.md §8.

## 2. Quyết định nền tảng

Các mục đánh dấu *(giả định)* là mặc định khi chưa có quyết định khác của chủ dự án; đổi được mà không ảnh hưởng kiến trúc.

| Vấn đề | Quyết định |
|---|---|
| Vị trí code | Thư mục `portal/` trong repo này *(giả định)* |
| Backend | Go (cùng phiên bản với `control-plane/go.mod`), module riêng `portal/`. Router `net/http` chuẩn (giống control-plane) |
| Frontend | React 19 + TypeScript + Vite trong `portal/web/`, tái dùng style/component của `console/` khi hợp lý |
| Database | PostgreSQL 16, migration SQL thuần (`portal/migrations/NNNN_*.sql`), bật Row Level Security |
| Cache / rate limit | Postgres trước; Redis chỉ thêm khi đo thấy cần |
| Identity provider | Zitadel tự host *(giả định)*; mọi code chỉ phụ thuộc OIDC chuẩn để đổi sang Keycloak/Auth0/Clerk được |
| Session web | Mô hình BFF: cookie `HttpOnly; Secure; SameSite=Lax`, token OIDC chỉ nằm ở server |
| Domain | Dashboard + API cùng origin tại `chat.devhub.io.vn` (đã quyết định, 2026-11-11). Edge: Traefik trên k3s-1 + cert-manager (Let's Encrypt), CI/CD `.github/workflows/deploy-k3s-portal.yml` + `deploy/k3s-1-portal-values.yaml`. Tách `api.<domain>` riêng chỉ khi cần |
| Phạm vi ban đầu | Multi-tenant ngay từ đầu, nhưng billing (S6) chỉ làm khi mở cho khách ngoài |
| Deploy | Helm chart `deploy/helm/portal`, cùng cluster với sandboxd |

## 3. Kiến trúc

```
                 OIDC login
 Người dùng ───────────────────▶ Zitadel
     │ cookie
     ▼
 ┌──────────────── Portal ────────────────┐        ┌──────── sandboxd (nội bộ) ────────┐
 │ Dashboard + BFF  (app.<domain>)        │        │ Console admin (chỉ VPN)           │
 │ API /v1 + MCP    (api.<domain>)  ──────┼─(4)───▶│ Control plane /v1                 │
 │ Postgres (RLS theo org_id)  ◀──────────┼─(5)────│   policy, approvals, audit        │
 └────────────────────────────────────────┘        │ Worker fleet → sandbox, egress    │
     ▲ (3) API key / agent token                   └───────────────────────────────────┘
     │                                                          ▲
 Hệ đa tác nhân ────────────────(6) key TTL ngắn gắn sandbox_id ┘ (tuỳ chọn, xem 3.3)
```

(3) agent gọi Portal; (4) Portal gọi sandboxd bằng service key; (5) sandboxd báo usage về Portal; (6) data plane trực tiếp, chỉ khi cần.

### 3.1 Danh tính người dùng (dashboard)

- Login: Authorization Code + PKCE với Zitadel. Callback ở BFF; BFF tạo session trong Postgres (`web_sessions`), trả cookie `portal_session` (32 byte ngẫu nhiên, lưu hash).
- CSRF: double-submit token hoặc header `X-CSRF-Token` cho mọi request thay đổi trạng thái từ trình duyệt.
- Lần đăng nhập đầu: tạo `users` từ claim `sub` + `email`, tạo org cá nhân, user là `owner`.
- Role trong org: `owner`, `admin`, `member`, `billing`. Role này **không liên quan** tới role `admin/operator/viewer` của sandboxd.

### 3.2 Danh tính agent

- **API key** thuộc một org, có thể giới hạn vào một project. Định dạng: `sbx_live_<keyid>_<secret>`, `keyid` 12 ký tự base32, `secret` 32 byte ngẫu nhiên base62. Lưu `keyid` và `sha256(secret)`; so sánh constant-time.
- Thuộc tính key: `scopes`, `project_id` (nullable), `expires_at` (nullable), `ip_allowlist` (nullable), `last_used_at`, `revoked_at`.
- **Agent token**: `POST /v1/tokens` dùng API key để lấy JWT (EdDSA, TTL mặc định 15 phút, tối đa 60) với claim:
  `sub` (key id), `org_id`, `project_id`, `scopes` (tập con của key), `max_sandboxes`, `agent_run_id`, `exp`, `jti`.
  Orchestrator phát token này cho từng sub-agent. Token không gia hạn được; hết hạn thì xin lại.
- Thu hồi: key bị thu hồi phải bị từ chối trong ≤ 60 giây (cache key tối đa 60 s). Token đã phát từ key bị thu hồi bị từ chối nhờ kiểm tra `revoked_at` của key cha (cache cùng TTL).

### 3.3 Portal → sandboxd

- Portal dùng **một** API key sandboxd (tạo qua `POST /v1/api-keys`, role `operator`, scopes `sandboxes:read`, `sandboxes:write`, `sandboxes:exec`, `sandboxes:egress`, `snapshots`). Lưu trong Kubernetes Secret, có runbook xoay vòng.
- Mọi sandbox Portal tạo đều mang tag tenant (xem 4.1).
- **Data plane trực tiếp (tuỳ chọn, S3b)**: với tác vụ dài, Portal gọi `POST /v1/api-keys` của sandboxd với `sandbox_id` + `ttl_seconds` (300–600) + scopes hẹp, rồi trả key đó cho agent. Key này chỉ dùng được cho đúng sandbox đó. Mặc định ở S3, Portal proxy `exec` luôn; chỉ làm S3b nếu đo thấy proxy là nút thắt.

### 3.4 Mô hình dữ liệu (Postgres của Portal)

```
users(id, oidc_sub UNIQUE, email, name, created_at)
orgs(id, slug UNIQUE, name, plan_id, created_at)
memberships(org_id, user_id, role, PRIMARY KEY(org_id, user_id))
invites(id, org_id, email, role, token_hash, expires_at, accepted_at)
projects(id, org_id, slug, name, created_at, UNIQUE(org_id, slug))
api_keys(id, org_id, project_id NULL, name, key_id UNIQUE, secret_hash, scopes text[],
         ip_allowlist cidr[] NULL, expires_at NULL, last_used_at, revoked_at NULL, created_by)
sandboxes(id /* = sandbox id của sandboxd */, org_id, project_id, created_by_key, agent_run_id,
          status, vcpu, memory_mb, created_at, stopped_at NULL)
usage_events(id, org_id, project_id, sandbox_id, kind, quantity, unit, occurred_at, source_event_id UNIQUE)
plans(id, name, max_concurrent_sandboxes, max_vcpu_per_sandbox, max_ttl_seconds,
      monthly_vcpu_seconds, egress_mode, rate_limit_rps)
idempotency_keys(org_id, key, request_hash, response_status, response_body, created_at, PRIMARY KEY(org_id, key))
audit_events(id, org_id, actor_type, actor_id, action, target, metadata jsonb, occurred_at)
web_sessions(id_hash, user_id, csrf_hash, expires_at)
```

- Mọi bảng có `org_id` bật RLS: `USING (org_id = current_setting('app.org_id')::uuid)`. Mỗi request mở transaction và `SET LOCAL app.org_id`. Role DB của app **không** phải owner bảng (để RLS có hiệu lực).
- Bảng `sandboxes` là chỉ mục phía Portal để kiểm tra quyền sở hữu nhanh và tính quota; nguồn sự thật về trạng thái vẫn là sandboxd.

## 4. Thay đổi cần làm trong sandboxd

Nhỏ, đặt trong một PR riêng (S3a) trước khi Portal gọi tới.

1. **Tag org.** Store đã có cột `external_workspace_id` (dùng ở API legacy `/sandbox`). Đưa nó vào API v1: thêm `WorkspaceID string json:"workspace_id,omitempty"` vào `v1External` (`control-plane/internal/api/v1.go`), ghi khi tạo, trả về khi đọc. Portal map: `workspace_id = org_id`, `project_id = project_id`, `user_id = api key id hoặc user id`.
2. **Lọc theo org.** `GET /v1/sandboxes?external_workspace_id=` (bổ sung vào `ListFiltered`), áp dụng cả với sandbox chạy trên worker.
3. **Bắt buộc tag với key của Portal.** Thêm thuộc tính cho API key (ví dụ cờ `require_external` hoặc scope `tenant:required`): key có cờ này tạo sandbox mà thiếu `external.workspace_id` thì bị 400.
4. **Webhook lifecycle.** Cấu hình `SANDBOXD_WEBHOOK_URL` + `SANDBOXD_WEBHOOK_SECRET`. Khi sandbox `created`, `started`, `stopped`, `deleted`, `failed`: POST JSON `{id, event, sandbox_id, external, resources, occurred_at}` kèm header `X-Sandboxd-Signature: sha256=<hmac(body)>` và `X-Sandboxd-Event-Id`. Retry có backoff, tối đa 24 giờ. Có thể dựng trên `internal/events` sẵn có.
5. **Không thay đổi** policy, approvals, egress, audit: Portal chỉ chọn preset theo gói, sandboxd vẫn là nơi thực thi.

## 5. API công khai của Portal (`api.<domain>`)

Auth: `Authorization: Bearer <api key hoặc agent token>`. Lỗi theo dạng `{"error":{"code":"...","message":"..."}}` giống control-plane v1.

| Endpoint | Việc | Scope |
|---|---|---|
| `POST /v1/tokens` | Đổi API key lấy agent token | chỉ API key, không nhận token |
| `POST /v1/sandboxes` | Tạo sandbox (image trong allowlist, resources, ttl, egress preset, ports, workspace repo) | `sandbox:create` |
| `GET /v1/sandboxes` | Liệt kê sandbox của org/project | `sandbox:read` |
| `GET /v1/sandboxes/{id}` | Chi tiết | `sandbox:read` |
| `POST /v1/sandboxes/{id}/exec` | Chạy lệnh, trả stdout/stderr/exit code | `sandbox:exec` |
| `GET /v1/sandboxes/{id}/logs` | Log | `sandbox:exec` |
| `POST /v1/sandboxes/{id}/stop`, `/start` | Dừng, chạy lại | `sandbox:create` |
| `DELETE /v1/sandboxes/{id}` | Huỷ | `sandbox:create` |
| `POST /v1/sandboxes/{id}/session` | (S3b) cấp key data plane TTL ngắn | `sandbox:exec` |
| `GET /v1/usage` | Usage theo kỳ | `usage:read` |

Quy tắc:

- Sandbox không thuộc org của caller → **404** (không phải 403, để không lộ sự tồn tại).
- `Idempotency-Key` bắt buộc hỗ trợ trên `POST /v1/sandboxes` và `exec`; lưu 24 giờ.
- Request tạo sandbox bị clamp theo gói (`plans`) trước, rồi sandboxd clamp tiếp theo policy instance.
- Rate limit theo key: token bucket, vượt → **429** kèm `Retry-After`. Vượt quota → **429** với `code: "quota_exceeded"`.
- File: control-plane v1 hiện không có API file; giai đoạn đầu đọc/ghi file qua `exec` (ví dụ `cat`, heredoc base64). Thêm endpoint file khi sandboxd có.

Dashboard API (`app.<domain>/api/...`) là API riêng của BFF, không công khai, không cần ổn định.

## 6. Milestones

Mỗi milestone một PR vào `main`. Không gộp. Test nằm cùng PR.

### S0 — Khung dự án
- `portal/` (Go module), `portal/web/` (Vite + React 19 + TS), `portal/migrations/`.
- `cmd/portal`: đọc config từ env, `/healthz`, `/readyz` (kiểm tra Postgres), log JSON.
- `docker-compose.portal.yml` cho dev: Postgres + Portal + Zitadel.
- Helm chart `deploy/helm/portal`; job CI build + test riêng cho `portal/`.
- **Xong khi:** `docker compose up` chạy được; CI xanh; chart render được (`helm template`).

### S1 — Đăng nhập và tenant
- OIDC login/logout qua BFF, `web_sessions`, CSRF.
- Bảng `users`, `orgs`, `memberships`, `invites`, `projects`, `audit_events` + RLS.
- Dashboard: login, tạo/đổi org, mời thành viên, đổi role, tạo project.
- **Xong khi:** test tích hợp chứng minh user org A không đọc/ghi được dữ liệu org B kể cả khi gọi thẳng repository mà quên `WHERE org_id` (RLS chặn); test CSRF; test cookie flags.

### S2 — API key và agent token
- Tạo / liệt kê / thu hồi key trên dashboard (hiển thị secret một lần).
- Middleware xác thực Bearer cho `api.<domain>`; cache 60 s.
- `POST /v1/tokens`, ký EdDSA, khoá xoay vòng được (`kid`).
- **Xong khi:** key thu hồi bị từ chối ≤ 60 s; token không xin được scope rộng hơn key; token hết hạn bị 401; test fuzz parse key không panic.

### S3a — Thay đổi sandboxd (mục 4)
- Làm 4 thay đổi ở mục 4 trong `control-plane/`, cập nhật `docs/openapi.yaml`.
- **Xong khi:** test Go cho tag `workspace_id`, filter, cờ bắt buộc tag, chữ ký webhook; CI hiện có vẫn xanh.

### S3 — Gateway tới sandboxd
- Client sandboxd trong Portal (`portal/internal/sandboxd`), timeout và retry hợp lý.
- Các endpoint ở mục 5 (trừ `/session`), kiểm tra sở hữu qua bảng `sandboxes`, idempotency.
- Nhận webhook (mục 4.4), xác minh HMAC, cập nhật trạng thái `sandboxes`.
- Script mẫu `portal/examples/agent.py`: tạo sandbox → exec `python -c 'print(1+1)'` → đọc kết quả → huỷ.
- **Xong khi:** script mẫu chạy được trên môi trường dev; test e2e: key org A gọi `exec` vào sandbox org B → 404; gửi lại cùng `Idempotency-Key` không tạo sandbox thứ hai.

### S3b — Data plane trực tiếp (chỉ khi cần)
- `POST /v1/sandboxes/{id}/session` → gọi sandboxd `POST /v1/api-keys` với `sandbox_id`, TTL 300–600 s.
- **Xong khi:** key trả về dùng được cho đúng sandbox và bị 403/404 với sandbox khác; hết TTL bị từ chối.

### S4 — Quota, rate limit, usage
- `plans` + gán gói cho org; kiểm tra quota trước khi tạo sandbox (đếm sandbox đang chạy trong transaction, khoá theo org).
- Rate limit theo key.
- Từ webhook, ghi `usage_events` (`vcpu_seconds`, `sandbox_seconds`), idempotent theo `source_event_id`.
- Trang Usage trên dashboard; `GET /v1/usage`.
- **Xong khi:** tạo vượt `max_concurrent_sandboxes` → 429 `quota_exceeded`; 50 request song song không vượt được quota (test race); usage khớp thời gian chạy ±1 phút.

### S5 — SDK và MCP
- `portal/openapi.yaml` là nguồn sự thật; sinh SDK Python và TypeScript (mỏng, có retry + idempotency tự động).
- MCP server ở `api.<domain>/mcp` (Streamable HTTP), tools: `create_sandbox`, `run_command`, `read_file`, `write_file`, `delete_sandbox`. Auth bằng Bearer API key/agent token.
- `docs/portal-quickstart.md`.
- **Xong khi:** một agent dùng MCP client chạy được code trong sandbox mà không viết HTTP tay; SDK có test với server giả.

### S6 — Billing và vận hành (chỉ khi mở cho khách ngoài)
- Stripe customer + subscription + metered usage đẩy hằng ngày từ `usage_events`.
- Gói free/pro; gói free bắt buộc xác minh email.
- Khoá console sandboxd sau VPN/IP allowlist; worker pool riêng (`pool=dedicated`) cho gói cao.
- **Xong khi:** đổi gói cập nhật quota ngay; webhook Stripe có xác minh chữ ký.

## 7. Bảo mật (checklist cho mọi PR)

- [ ] Không log secret, API key, token, cookie. Log chỉ in `key_id`.
- [ ] Mọi truy vấn chạy trong transaction có `SET LOCAL app.org_id`.
- [ ] Mọi endpoint có `{id}` kiểm tra sở hữu, sai → 404.
- [ ] Sandbox bị chặn egress tới dải IP nội bộ của cluster (Portal, sandboxd, Postgres, Zitadel, metadata `169.254.169.254`). Kiểm tra bằng test e2e từ S3.
- [ ] Webhook vào và ra đều ký HMAC và chống replay (timestamp ±5 phút + event id).
- [ ] Prefix `sbx_live_` để secret scanning nhận diện được; đăng ký pattern nếu repo public.
- [ ] Header bảo mật trên dashboard: CSP, `X-Frame-Options: DENY`, HSTS khi có TLS.

## 8. Ngoài phạm vi

- Viết lại policy/approvals/egress trong Portal (dùng của sandboxd).
- Mỗi tenant một sandboxd riêng.
- Agent framework, prompt, model provider: Portal chỉ là hạ tầng chạy code.

## 9. Câu hỏi còn mở

Các câu dưới đây đã có giả định ở mục 2; trả lời khác thì cập nhật mục 2 rồi mới làm milestone liên quan.

1. Portal chỉ phục vụ agent nội bộ, hay mở đăng ký cho khách ngoài? (ảnh hưởng S1 tự đăng ký và S6)
2. Zitadel tự host hay IdP managed?
3. `portal/` trong repo này hay repo riêng?
4. Tên miền cụ thể cho `app.` và `api.`. — **Đã chọn `chat.devhub.io.vn`** (2026-11-11): dashboard và API công khai cùng origin qua BFF, chạy trên k3s-1 (workflow `deploy-k3s-portal.yml`). Nếu sau này cần tách `api.<domain>` riêng, cập nhật mục 2 rồi mới làm milestone liên quan.
