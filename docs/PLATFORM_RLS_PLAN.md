# KẾ HOẠCH — Org nền tảng "Devhub Solutions", vai trò và RLS đúng nghĩa

Yêu cầu gốc (Doan, 2026-11-12):
> *"giờ cần lên plan để RLS cho đúng, Devhub Solutions là orgs nền tảng, quản lý các orgs và người đăng ký khác, người dùng trong Devhub Solutions có thể quản lý cấp tài nguyên, gói, hạn mức, còn người dùng của nền tảng nhận gói tạo agent và sử dụng"*

Tài liệu này chỉ là kế hoạch, chưa có code. Nó bổ sung cho `docs/PORTAL_PLAN.md` (mục 3.1 vai trò, 3.4 RLS) và `docs/AGENT_PLAN_A3.md`. Migration đầu tiên của kế hoạch này sẽ là `0011_platform_org.sql`.

---

## 0. Tóm tắt

1. **Hai loại org**: `orgs.kind = 'platform'` (đúng **một** org: Devhub Solutions) và `orgs.kind = 'customer'` (mọi org còn lại, gồm cả personal org tạo khi đăng nhập lần đầu).
2. **Nhân viên nền tảng = thành viên của org platform.** Không thêm bảng `platform_admins`. Vai trò trong org platform dùng lại 4 role sẵn có (`owner/admin/billing/member`) nhưng mang nghĩa nền tảng (mục 3).
3. **RLS biết ai là nhân viên** qua hàm `SECURITY DEFINER` `app_platform_role()`. Hàm đọc membership của `app.user_id` trong org platform. Quyền đến từ dữ liệu trong DB chứ không từ một GUC mà app tự set, nên một câu SQL lỗi không thể tự nâng quyền.
4. **Chế độ nền tảng = `app.org_id` rỗng.** Các nhánh policy cho phép đọc hoặc ghi chéo org chỉ mở khi `app.org_id` rỗng **và** `app_platform_role()` đủ mức. Khi nhân viên dùng trang của một org khách (có `app.org_id`), họ bị RLS giới hạn như người dùng thường. Đây là cùng cách 0009 đã làm cho `model_connections`.
5. **Gói, hạn mức, cấp tài nguyên tách khỏi `orgs`**: `org_subscriptions` (org đang dùng gói nào), `org_quota_grants` (cộng thêm hoặc ghi đè hạn mức) và `plan_llm_connections` (gói nào được dùng kết nối LLM nền tảng nào). Org khách chỉ **đọc** các bảng này. Chỉ nhân viên nền tảng được ghi.
6. **Vá 4 lỗ RLS hiện có** (mục 1.2, đã tái hiện trên Postgres 16): thành viên tự nâng gói, tự nâng role, tự chèn mình vào org khác, và bất kỳ ai cũng ghi được kết nối LLM nền tảng khi `app.org_id` rỗng.
7. **Khách hàng**: owner/admin quản lý thành viên, API key và kết nối LLM riêng. Member tạo sandbox, agent run và chat trong hạn mức gói. Không ai trong org khách sửa được gói hay hạn mức.

---

## 1. Hiện trạng (main @ `0e04fe8`, 2026-11-12)

### 1.1 Đang có

| Thành phần | Ở đâu | Ghi chú |
|---|---|---|
| GUC `app.org_id`, `app.user_id` | `internal/db/tx.go` | `WithTx`, `WithOrg`, `BootstrapTx`, `PinUser` |
| Role DB `portal_app` (không phải owner bảng) | 0002 | RLS áp dụng cho mọi query của app |
| Org + membership + 4 role | 0002, `internal/tenants` | `owner/admin/member/billing`, kiểm tra trong Go bằng `requireMember` và `requireAdmin` |
| Gói | 0007 `plans` (không RLS, app chỉ `SELECT`) | Hiện là `orgs.plan_id` dạng soft FK, mặc định `'free'` |
| Platform admin | `requirePlatformAdmin` (`httpapi/llm_connections.go:281`) | Chỉ đúng **một** người: `BootstrapAdminID` (tài khoản password bootstrap). Chưa có khái niệm org nền tảng |
| Kết nối LLM nền tảng | 0008/0009, `org_id IS NULL` | Ghi khi `app.org_id` rỗng |
| Cửa chéo org | `api_key_verify`, `agent_claim_next`, `agent_sweep_lost` (SECURITY DEFINER) | Có test riêng |
| Đăng ký | Đăng nhập OIDC lần đầu tạo personal org; `POST /api/orgs` cho ai cũng tạo org | Không có duyệt, không có trạng thái org |

### 1.2 Lỗ RLS đã tái hiện

Chạy toàn bộ migration 0001–0010 trên Postgres 16 local, rồi `SET ROLE portal_app` với `app.user_id` = một **member** của org A và `app.org_id` rỗng:

| # | Câu lệnh | Kết quả | Nguyên nhân |
|---|---|---|---|
| L1 | `UPDATE orgs SET plan_id='enterprise' WHERE id=A` | **UPDATE 1** | `orgs_tenant FOR ALL` có nhánh "là thành viên", và WITH CHECK ngầm bằng USING |
| L2 | `UPDATE memberships SET role='owner' WHERE user_id=me` | **UPDATE 1** | `memberships_tenant FOR ALL USING (user_id = app.user_id OR …)` |
| L3 | `INSERT INTO memberships VALUES (B, me, 'owner')` | **INSERT 1**: tự vào org B làm owner | Cùng policy: WITH CHECK chỉ cần `user_id = me` |
| L4 | `INSERT INTO model_connections (org_id NULL, …)` | **INSERT 1** | 0009 coi "app.org_id rỗng" là platform admin. Mọi tx `WithTx` không có org đều thỏa |

Hôm nay **API không cho làm những việc này**, vì Go kiểm tra role trước khi chạy SQL. Nhưng RLS là lớp thứ hai (PORTAL_PLAN 3.4) và ở đây nó không chặn gì. Chỉ cần một handler mới quên `requireAdmin`, hoặc một câu SQL ghép sai, là thành viên tự lên gói enterprise hoặc chiếm org khác. Khi có org nền tảng với quyền cấp gói, lỗ L1 và L3 trở thành lỗ leo thang quyền thật. Vì vậy phải vá trước khi làm phần quản trị (R0 đứng đầu, mục 7).

---

## 2. Mô hình

```
                      ┌──────────────────────────────────────┐
                      │  org "Devhub Solutions" kind=platform│
                      │  owner / admin / billing / member    │──── quản lý ────┐
                      └──────────────────────────────────────┘                 │
                                                                                ▼
 plans ──< org_subscriptions >── orgs(kind=customer) ──< memberships >── users
   │                                │  ▲
   └──< plan_llm_connections        │  └── org_quota_grants (cấp thêm / ghi đè hạn mức)
          >── model_connections     │
              (org_id NULL)         ├──< api_keys, projects, sandboxes, agent_runs,
                                    │    chat_*, model_connections (org), usage_events
                                    └── status: pending | active | suspended
```

- **Org platform** là một org bình thường về dữ liệu (nhân viên vẫn chat, tạo agent, dùng sandbox trong đó như khách), cộng thêm **quyền nền tảng** cho các thành viên của nó.
- **Gói (plan)** là catalog do nền tảng sở hữu. Một org khách có đúng một subscription đang hiệu lực.
- **Hạn mức hiệu lực** = giới hạn của gói, áp các grant còn hạn lên trên (mục 4.4). Hàm `org_effective_limits(org)` trả về kết quả này. `internal/quota` chuyển từ đọc `plans` sang đọc hàm đó.
- **"Cấp tài nguyên"** gồm: (a) grant hạn mức (thêm vCPU-giây, thêm sandbox đồng thời, nâng TTL…), (b) gói nào được dùng kết nối LLM nền tảng nào (`plan_llm_connections`), và (c) để dành cho sau: gán node pool riêng cho org (cột `node_pool` trong grant, ánh xạ sang tag của sandboxd).

---

## 3. Vai trò và ma trận quyền

### 3.1 Trong org platform (nhân viên Devhub)

| Role | Tên hiển thị | Được làm |
|---|---|---|
| `owner` | Platform owner | Mọi thứ của admin, cộng thêm: quản lý nhân viên (thêm/bớt/đổi role trong org platform), sửa cài đặt nền tảng (chế độ đăng ký, gói mặc định) |
| `admin` | Platform admin | Duyệt, tạm khóa, mở khóa org; gán và đổi gói; tạo grant hạn mức; CRUD catalog gói; CRUD kết nối LLM nền tảng; xem usage mọi org |
| `billing` | Billing | Xem mọi org; gán và đổi gói; CRUD catalog gói; xem usage. **Không** grant hạn mức ngoài gói, **không** sửa kết nối LLM |
| `member` | Support (chỉ đọc) | Xem danh sách org, thành viên, gói, hạn mức, usage, metadata sandbox và agent run. Không ghi gì |

Nhân viên **không** đọc được nội dung riêng của khách (`chat_*`, `agent_messages`, `agent_events`, file trong sandbox) ở chế độ nền tảng. Nếu cần hỗ trợ sâu thì dùng "break-glass": khách mời nhân viên vào org (invite bình thường). Cách này để lại dấu vết audit trong org khách và khách tự gỡ được. Không làm cửa sau trong phạm vi kế hoạch này.

### 3.2 Trong org khách (người dùng nền tảng)

| Hành động | owner | admin | member | billing |
|---|:-:|:-:|:-:|:-:|
| Sửa tên org | ✓ | ✓ | | |
| Mời, đổi role, gỡ thành viên (không đụng owner, không tự đổi mình) | ✓ | ✓ | | |
| API key, kết nối LLM riêng của org | ✓ | ✓ | | |
| Tạo/dùng sandbox, agent run, chat (trong hạn mức) | ✓ | ✓ | ✓ | |
| Xem gói, hạn mức hiệu lực, usage | ✓ | ✓ | ✓ | ✓ |
| Đổi gói, sửa hạn mức, đổi trạng thái org | | | | |

Dòng cuối để trống là có chủ ý: đổi gói chỉ qua nền tảng. Tự nâng cấp qua thanh toán là việc của S6 (billing), nằm ngoài kế hoạch này.

---

## 4. Thay đổi schema — `0011_platform_org.sql`

### 4.1 Loại org và trạng thái

```sql
ALTER TABLE orgs
  ADD COLUMN kind   TEXT NOT NULL DEFAULT 'customer' CHECK (kind IN ('platform','customer')),
  ADD COLUMN status TEXT NOT NULL DEFAULT 'active'   CHECK (status IN ('pending','active','suspended')),
  ADD COLUMN status_reason TEXT NOT NULL DEFAULT '',
  ADD COLUMN status_changed_at TIMESTAMPTZ NULL;

-- Đúng một org platform.
CREATE UNIQUE INDEX orgs_one_platform ON orgs ((true)) WHERE kind = 'platform';
```

Seed org platform **không** nằm trong migration (slug và owner tùy môi trường). Lúc khởi động, `EnsurePlatformOrg` (giống `passwordauth.EnsureBootstrap`) đọc env `PORTAL_PLATFORM_ORG_SLUG` (mặc định `devhub`) và `PORTAL_PLATFORM_ORG_NAME` (mặc định `Devhub Solutions`), tạo org `kind='platform'` nếu chưa có, rồi gán bootstrap admin làm `owner`. Lệnh này chạy bằng login migration (owner bảng), không qua `portal_app`.

### 4.2 Hàm nhận diện nhân viên (cửa duy nhất)

```sql
-- Role của app.user_id trong org platform, hoặc NULL.
CREATE FUNCTION app_platform_role() RETURNS TEXT
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public AS $$
  SELECT m.role FROM memberships m JOIN orgs o ON o.id = m.org_id
  WHERE o.kind = 'platform'
    AND m.user_id = NULLIF(current_setting('app.user_id', true), '')::uuid
$$;

-- Chế độ nền tảng: không có org đang thao tác VÀ là nhân viên đủ mức.
-- p_need: 'read' (mọi role) | 'billing' (owner, admin, billing) | 'admin' (owner, admin) | 'owner'
CREATE FUNCTION app_platform_can(p_need TEXT) RETURNS BOOLEAN
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public AS $$
  SELECT NULLIF(current_setting('app.org_id', true), '') IS NULL
     AND CASE p_need
           WHEN 'read'    THEN app_platform_role() IS NOT NULL
           WHEN 'billing' THEN app_platform_role() IN ('owner','admin','billing')
           WHEN 'admin'   THEN app_platform_role() IN ('owner','admin')
           WHEN 'owner'   THEN app_platform_role() = 'owner'
           ELSE false
         END
$$;

-- Role của app.user_id trong org đang thao tác (app.org_id), hoặc NULL.
CREATE FUNCTION app_org_role() RETURNS TEXT
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public AS $$
  SELECT role FROM memberships
  WHERE org_id  = NULLIF(current_setting('app.org_id', true), '')::uuid
    AND user_id = NULLIF(current_setting('app.user_id', true), '')::uuid
$$;
REVOKE ALL ON FUNCTION app_platform_role(), app_platform_can(TEXT), app_org_role() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app_platform_role(), app_platform_can(TEXT), app_org_role() TO portal_app;
```

Các hàm cần `SECURITY DEFINER` để tránh đệ quy policy (`memberships` cũng có RLS). Trong policy luôn gọi hàm dưới dạng `(SELECT app_platform_can('read'))` để Postgres tính một lần mỗi câu lệnh (initplan) thay vì một lần mỗi dòng.

### 4.3 Gói

```sql
ALTER TABLE plans
  ADD COLUMN is_public BOOLEAN NOT NULL DEFAULT true,   -- false = gói riêng, chỉ org được gán mới thấy
  ADD COLUMN is_active BOOLEAN NOT NULL DEFAULT true,   -- false = không gán mới, org đang dùng giữ nguyên
  ADD COLUMN monthly_llm_tokens BIGINT NOT NULL DEFAULT 0,  -- 0 = không giới hạn ở tầng gói
  ADD COLUMN monthly_agent_runs INT    NOT NULL DEFAULT 0,
  ADD COLUMN max_members        INT    NOT NULL DEFAULT 0,
  ADD COLUMN max_api_keys       INT    NOT NULL DEFAULT 0,
  ADD COLUMN updated_at TIMESTAMPTZ NOT NULL DEFAULT now();

CREATE TABLE org_subscriptions (
  org_id      UUID PRIMARY KEY REFERENCES orgs(id) ON DELETE CASCADE,
  plan_id     TEXT NOT NULL REFERENCES plans(id),
  status      TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('trial','active','past_due','cancelled')),
  period_start TIMESTAMPTZ NOT NULL DEFAULT date_trunc('month', now()),
  period_end   TIMESTAMPTZ NULL,
  assigned_by UUID NULL REFERENCES users(id),
  note        TEXT NOT NULL DEFAULT '',
  updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- Backfill: INSERT INTO org_subscriptions (org_id, plan_id) SELECT id, plan_id FROM orgs;
-- orgs.plan_id giữ lại một phiên bản (đọc-only, đồng bộ bằng trigger) rồi drop ở R5.

CREATE TABLE plan_llm_connections (
  plan_id       TEXT NOT NULL REFERENCES plans(id) ON DELETE CASCADE,
  connection_id UUID NOT NULL REFERENCES model_connections(id) ON DELETE CASCADE,
  PRIMARY KEY (plan_id, connection_id)
);
```

Nếu một kết nối nền tảng không có dòng nào trong `plan_llm_connections` thì mọi gói đều dùng được (giữ hành vi hiện tại). Khi có ít nhất một dòng, chỉ các gói được liệt kê mới dùng được kết nối đó.

### 4.4 Cấp tài nguyên / hạn mức

```sql
CREATE TABLE org_quota_grants (
  id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id      UUID NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  limit_key   TEXT NOT NULL CHECK (limit_key IN (
                'max_concurrent_sandboxes','max_vcpu_per_sandbox','max_ttl_seconds',
                'monthly_vcpu_seconds','monthly_llm_tokens','monthly_agent_runs',
                'max_members','max_api_keys','rate_limit_rps','egress_mode','node_pool')),
  mode        TEXT NOT NULL CHECK (mode IN ('add','set')),
  value       TEXT NOT NULL,             -- số hoặc chuỗi (egress_mode, node_pool); hàm effective ép kiểu
  starts_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  expires_at  TIMESTAMPTZ NULL,
  reason      TEXT NOT NULL,             -- bắt buộc, hiện trong audit
  granted_by  UUID NOT NULL REFERENCES users(id),
  revoked_at  TIMESTAMPTZ NULL,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX org_quota_grants_live ON org_quota_grants (org_id) WHERE revoked_at IS NULL;
```

`org_effective_limits(p_org UUID)` (STABLE, `SECURITY INVOKER`, nên chạy dưới RLS của người gọi) tính như sau: lấy dòng gói của subscription, áp các grant còn hạn theo thứ tự `set` trước rồi `add`, và trả về một dòng. Org khách gọi với org của mình. Nhân viên gọi được ở chế độ nền tảng. Ai khác gọi thì không có dòng (RLS lọc mất).

Grant luôn có `reason` và `granted_by`. Thu hồi bằng `revoked_at`, không xóa. Lịch sử giữ nguyên để đối soát.

### 4.5 Cài đặt nền tảng

```sql
CREATE TABLE platform_settings (
  key   TEXT PRIMARY KEY CHECK (key IN ('signup_mode','default_plan_id','personal_org_on_login')),
  value TEXT NOT NULL,
  updated_by UUID NULL REFERENCES users(id),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- seed: signup_mode='open' (open | approval | invite_only), default_plan_id='free', personal_org_on_login='true'
```

---

## 5. Policy RLS (thay toàn bộ policy `FOR ALL` lỏng)

Quy ước: `ORG` = `NULLIF(current_setting('app.org_id', true), '')::uuid`, `ME` = `…('app.user_id'…)::uuid`, `P(x)` = `(SELECT app_platform_can('x'))`, `R` = `(SELECT app_org_role())`.

Policy tách theo lệnh (SELECT/INSERT/UPDATE/DELETE) để USING và WITH CHECK luôn được viết rõ ràng, không để Postgres ngầm suy ra.

### 5.1 `orgs`

| Lệnh | Điều kiện |
|---|---|
| SELECT | `id = ORG` OR thành viên (như cũ) OR `P(read)` |
| INSERT | (`id = ORG` AND `kind = 'customer'` AND `status` hợp với `signup_mode`) OR `P(admin)`. `kind='platform'` không bao giờ insert được qua `portal_app` (chỉ `EnsurePlatformOrg`) |
| UPDATE | (`id = ORG` AND `R IN (owner, admin)`) OR `P(admin)` |
| DELETE | `P(owner)` (khách không tự xóa org trong phạm vi này) |

Thêm trigger `BEFORE UPDATE orgs_guard`: nếu `kind`, `status`, `status_reason` hoặc `plan_id` thay đổi mà `NOT app_platform_can('admin')` thì `RAISE 42501`. Cột `kind` không bao giờ đổi được, kể cả với nhân viên. Trigger cần thiết vì policy theo dòng không phân biệt được cột nào đang bị sửa. Đây là cách vá **L1**.

### 5.2 `memberships` (vá **L2**, **L3**)

| Lệnh | Điều kiện |
|---|---|
| SELECT | `user_id = ME` OR `org_id = ORG` OR `P(read)` |
| INSERT | (`org_id = ORG` AND `R IN (owner, admin)` AND `role <> 'owner'`) OR **org mới chưa có thành viên** (`org_id = ORG` AND `user_id = ME` AND `role='owner'` AND `NOT EXISTS (… memberships của ORG)`, qua hàm DEFINER `app_org_is_empty()`), OR `P(owner)` khi `org_id` là org platform, OR `P(admin)` khi `org_id` là org khách |
| UPDATE | `org_id = ORG` AND `R IN (owner, admin)` AND `user_id <> ME` AND role cũ và mới đều `<> 'owner'`, OR như INSERT cho chế độ nền tảng |
| DELETE | như UPDATE, cộng thêm `user_id = ME AND role <> 'owner'` (tự rời org) |

Chấp nhận invite **chuyển hẳn sang** hàm DEFINER `accept_invite(org, token, user, email)`. Hàm kiểm tra token hash, hạn, email người được mời, rồi chèn membership. Không còn đường INSERT membership nào chỉ dựa vào "user_id là mình".

Membership của **org platform** chỉ do platform owner quản lý (`P(owner)`). Platform admin không tự thêm người vào đội nhân viên được.

### 5.3 Bảng do nền tảng sở hữu

| Bảng | SELECT | INSERT / UPDATE / DELETE |
|---|---|---|
| `plans` | `is_public` OR gói đang được gán cho `ORG` OR `P(read)` | `P(billing)` |
| `org_subscriptions` | `org_id = ORG` OR `P(read)` | `P(billing)` |
| `org_quota_grants` | `org_id = ORG` OR `P(read)` | INSERT, UPDATE (chỉ để thu hồi): `P(admin)`. Không có DELETE |
| `plan_llm_connections` | `P(read)` OR gói của `ORG` | `P(admin)` |
| `platform_settings` | `P(read)` (app đọc `signup_mode` qua hàm DEFINER `platform_signup_mode()`) | `P(owner)` |
| `model_connections` dòng `org_id IS NULL` | như 0009 (mọi org đọc metadata) | `P(admin)` thay cho "app.org_id rỗng". Đây là cách vá **L4** |

`plans` bật RLS (hiện chưa có) và cấp thêm `INSERT, UPDATE` cho `portal_app`. Không cấp `DELETE`: gói thôi bán thì đặt `is_active=false`.

### 5.4 Bảng tenant: thêm nhánh đọc cho nhân viên

Mỗi bảng thêm **một** policy `FOR SELECT` (permissive, cộng OR với policy cũ) `USING (P(read))`:

- `projects`, `api_keys` (handler admin chỉ chọn cột metadata, không lấy `secret_hash`), `sandboxes`, `usage_events`, `agent_runs` (handler admin không trả `prompt` và `result`), `invites`, `audit_events`.

**Không** thêm nhánh nhân viên cho: `chat_conversations`, `chat_messages`, `agent_events`, `agent_messages`, `model_connection_keys`, `idempotency_keys`. Đây là nội dung riêng của khách hoặc bí mật.

Không bảng tenant nào có nhánh **ghi** cho nhân viên. Nếu nhân viên cần dừng sandbox của org bị khóa, việc đó đi qua sandboxd bằng service key và handler ghi audit. Dòng `sandboxes` được cập nhật bằng `WithOrg(target)` từ webhook như hiện nay.

### 5.5 Org bị khóa

`orgs.status <> 'active'` thì:
- Go: middleware session và Bearer trả `403 org_suspended` hoặc `org_pending` cho mọi route `/api/orgs/{id}/*` và `/v1/*`, trừ `GET` thông tin org, gói và usage (để khách biết vì sao bị khóa).
- DB (lớp hai): `api_key_verify` thêm điều kiện `JOIN orgs … status = 'active'`. `agent_claim_next` bỏ qua run của org không active.
- Khi khóa org: handler gọi sandboxd dừng mọi sandbox của org (tag `external.org_id`) và hủy các agent run đang chạy.

### 5.6 Audit

Mọi thao tác ở chế độ nền tảng ghi `audit_events` với `org_id = org bị tác động`, `actor_type = 'platform'` và `metadata.platform_role`. Khách thấy được nền tảng đã làm gì với org mình. Thao tác không gắn với org khách nào (sửa gói, sửa cài đặt) ghi vào `org_id = org platform`. Policy INSERT của `audit_events` thêm nhánh `P(read)` để nhân viên ghi được log cho org khác. SELECT vẫn như mục 5.4.

---

## 6. Thay đổi ở tầng Go

| Thành phần | Thay đổi |
|---|---|
| `internal/db` | Thêm `WithPlatform(ctx, pool, fn)`: tx với `app.org_id=''` và `app.user_id`. Đặt tên rõ để handler nền tảng không dùng nhầm `WithTx` |
| `requirePlatformAdmin` | Thay so sánh `BootstrapAdminID` bằng `requirePlatform(need)`, gọi `app_platform_role()`. Bootstrap admin vẫn vào được vì `EnsurePlatformOrg` gán họ làm owner |
| `requireAdmin` / `requireMember` | Giữ nguyên, thêm kiểm tra `orgs.status`. Personal org vẫn là `kind='customer'` |
| `bootstrapLogin`, `handleCreateOrg` | Đọc `platform_signup_mode()`: `open` thì org `active`; `approval` thì org `pending`; `invite_only` thì không tạo personal org, `POST /api/orgs` trả 403 với người không phải nhân viên. Org mới nhận subscription `default_plan_id` trong cùng tx |
| `internal/quota`, `internal/usage` | `LoadPlan` chuyển sang `org_effective_limits`. Thêm kiểm tra `monthly_agent_runs` và `monthly_llm_tokens` (A3.3 đã ghi `input_tokens`/`output_tokens` vào `agent_runs`) |
| `llmconfig` resolve | Kết nối nền tảng chỉ hợp lệ nếu gói của org được phép (`plan_llm_connections`) |
| `/me` | Trả thêm `platform_role` để web hiện menu "Nền tảng" |

### 6.1 API nền tảng mới (`/api/platform/*`, cookie, chế độ nền tảng)

| Route | Quyền |
|---|---|
| `GET /api/platform/orgs?status=&q=` | read |
| `GET /api/platform/orgs/{id}` (thành viên, subscription, grants, effective limits, usage tháng) | read |
| `POST /api/platform/orgs/{id}/status` `{status, reason}` | admin |
| `PUT /api/platform/orgs/{id}/subscription` `{plan_id, status, period_end, note}` | billing |
| `POST /api/platform/orgs/{id}/grants`, `DELETE …/grants/{gid}` (thu hồi) | admin |
| `GET/POST/PATCH /api/platform/plans`, `PUT /api/platform/plans/{id}/llm-connections` | billing (LLM: admin) |
| `GET/PUT /api/platform/settings` | read / owner |
| `GET/POST/PATCH/DELETE /api/platform/staff` (membership của org platform) | owner |
| `/api/admin/llm/*` | Đổi tên thành `/api/platform/llm/*`, giữ alias cũ một phiên bản |

### 6.2 Web

Thêm mục "Nền tảng" ở sidebar (chỉ hiện khi `platform_role` có giá trị), gồm các trang: Orgs (danh sách và chi tiết), Gói, Cấp hạn mức (bên trong trang chi tiết org), Kết nối LLM nền tảng (chuyển từ Admin LLM sang), Nhân viên, Cài đặt. Phía khách thêm trang "Gói & hạn mức" chỉ đọc (mở rộng trang Usage hiện có).

---

## 7. Phases

Mỗi phase là một PR, có test RLS chạy trên Postgres thật (`PORTAL_TEST_DATABASE_URL`, giống CI hiện tại).

### R0 — Vá RLS hiện có (không thêm tính năng)
- Tách policy `orgs` và `memberships` theo lệnh. Thêm trigger `orgs_guard` (tạm thời: chặn sửa `plan_id` qua `portal_app`). Chuyển accept invite sang hàm DEFINER.
- `model_connections`: tạm giữ "app.org_id rỗng" nhưng thêm `app.user_id = bootstrap admin` qua hàm `app_platform_role()` bản tạm (đọc `password_logins` bootstrap). R1 thay bằng org platform.
- Test: 4 câu L1–L4 ở mục 1.2 phải ra `42501` hoặc 0 dòng. Các luồng hiện có (đăng nhập lần đầu, tạo org, mời và nhận lời mời, đổi role) vẫn chạy.

### R1 — Org platform + vai trò nhân viên
- `orgs.kind/status`, `EnsurePlatformOrg`, `app_platform_role/can`, `WithPlatform`, `requirePlatform(need)`, `/api/platform/staff`, `/me.platform_role`.
- `/api/admin/llm/*` chuyển sang `P(admin)`.

### R2 — Gói và subscription
- `plans` thêm cột và RLS, `org_subscriptions` (backfill), `/api/platform/plans`, `PUT …/subscription`, `orgs.plan_id` chuyển thành cột chỉ đọc được trigger đồng bộ.
- `quota.LoadPlan` đọc subscription.

### R3 — Grant hạn mức và effective limits
- `org_quota_grants`, `org_effective_limits`, `/api/platform/orgs/{id}/grants`, quota check mới (`monthly_agent_runs`, `monthly_llm_tokens`, `max_members`, `max_api_keys`), `plan_llm_connections`.

### R4 — Đăng ký và trạng thái org
- `platform_settings`, `signup_mode`, duyệt và khóa org, middleware `org_suspended`, `api_key_verify` và `agent_claim_next` lọc theo status, dừng sandbox khi khóa.

### R5 — UI nền tảng + dọn dẹp
- Các trang ở mục 6.2. Drop `orgs.plan_id`, bỏ alias `/api/admin/llm/*`, bỏ `BootstrapAdminID` khỏi `Server`. Cập nhật `docs/PORTAL_PLAN.md` mục 3.1 và 3.4 cùng `openapi.yaml`.

Thứ tự bắt buộc: R0 → R1 → (R2, R3) → R4 → R5. R2 và R3 có thể làm song song nếu chia người.

---

## 8. Kiểm thử RLS

Một bảng test dạng ma trận (`internal/db/rls_matrix_test.go`) chạy với `SET ROLE portal_app`, gồm:

- **Tác nhân**: member, admin và owner của org khách A; user ngoài A; platform member, billing, admin, owner, mỗi người thử ở cả chế độ nền tảng lẫn khi đang thao tác trong org A.
- **Bảng × lệnh**: mọi bảng ở mục 5, mỗi bảng thử SELECT, INSERT, UPDATE và DELETE.
- **Kỳ vọng** viết thành bảng dữ liệu trong test, và chính bảng đó là tài liệu ma trận quyền.

Thêm các case bắt buộc:
1. L1–L4.
2. Nhân viên đang ở org A (có `app.org_id`) không đọc được org B.
3. Platform admin không thêm được nhân viên (chỉ owner).
4. Không ai đổi được `kind`.
5. Org `suspended` thì `api_key_verify` trả rỗng.
6. Nhân viên không đọc được `chat_messages`/`agent_messages` của khách ở bất kỳ chế độ nào.
7. `org_effective_limits` với grant `set` và `add`, grant hết hạn, grant đã thu hồi.

---

## 9. Rủi ro

| Rủi ro | Cách xử lý |
|---|---|
| Hàm DEFINER bị lợi dụng qua `search_path` | Mọi hàm `SET search_path = public`, `REVOKE … FROM PUBLIC`, không nhận tham số tự do làm điều kiện chéo org |
| Policy gọi hàm làm chậm query lớn (usage, list sandbox) | Gói trong `(SELECT …)` thành initplan. Đo `EXPLAIN` trên 100k `usage_events` trong R3 |
| Bootstrap admin mất quyền khi chuyển sang org platform | `EnsurePlatformOrg` chạy trước khi server nhận request và idempotent. Có test boot |
| Nhân viên vừa là khách của org khác | Chế độ nền tảng tách bằng `app.org_id` rỗng (mục 0 ý 4). Test case 2 |
| Migration backfill subscription trên DB live | `INSERT … ON CONFLICT DO NOTHING`, giữ `orgs.plan_id` đến R5 |

---

## 10. Câu hỏi cho Doan (đã có mặc định, R0–R1 làm được ngay)

1. **Chế độ đăng ký mặc định**: `open` (như hiện tại), `approval` (đăng ký xong chờ Devhub duyệt) hay `invite_only`? *Mặc định đề xuất: `approval`* cho bản public, vì nền tảng cấp tài nguyên thật.
2. **Personal org khi đăng nhập lần đầu** có giữ không? *Mặc định: giữ*, tạo ở trạng thái `pending` nếu chế độ là `approval`.
3. **Billing được grant hạn mức không?** *Mặc định: không* (chỉ admin). Billing chỉ đổi gói.
4. **Support có đọc metadata agent run (không có prompt) không?** *Mặc định: có*. Nội dung chat và prompt thì không.
5. **Slug org platform**: `devhub`? Bootstrap admin hiện tại sẽ là platform owner.
