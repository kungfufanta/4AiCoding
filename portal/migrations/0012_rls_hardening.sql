-- 0012_rls_hardening.sql — R0: vá 4 lỗ RLS (PLATFORM_RLS_PLAN §7 R0).
--
-- Không thêm tính năng, không động đến schema (kind, status, settings,
-- grant bảng là R1+). Chỉ:
--   * tách policy orgs/memberships theo lệnh (SELECT/INSERT/UPDATE/
--     DELETE), WITH CHECK viết rõ,
--   * thêm trigger orgs_guard chặn sửa plan_id qua portal_app (vá L1),
--   * chuyển accept invite sang SECURITY DEFINER accept_invite(),
--     bỏ đường INSERT membership chỉ dựa vào user_id = me (vá L2, L3),
--   * yêu cầu app_platform_role() (bản tạm: password_logins) để ghi
--     model_connections dòng org_id NULL (vá L4).
--
-- Lỗ (plan §1.2):
--   L1: UPDATE orgs SET plan_id='enterprise' — member tự nâng gói.
--   L2: UPDATE memberships SET role='owner' WHERE user_id=me — tự lên owner.
--   L3: INSERT INTO memberships VALUES (B, me, 'owner') — tự vào org khác.
--   L4: INSERT INTO model_connections (org_id NULL, ...) — bất kỳ tx
--       WithTx nào cũng ghi được platform connection.
--
-- Mọi hàm DEFINER: SET search_path = public (chống search_path injection),
-- REVOKE ALL FROM PUBLIC, GRANT EXECUTE TO portal_app. Owner hàm là
-- migration runner = table owner, nên hàm bypass RLS (RLS không FORCE).

-- === 1. Hàm DEFINER tạm: app_platform_role() (R0) ====================
-- R0: đọc password_logins để biết app.user_id có phải bootstrap admin.
-- R1 sẽ thay bằng membership trong org platform. Trả về 'admin' khi
-- app.user_id là một password login user, NULL khi không.
--
-- GIẢ ĐỊNH R0: mọi dòng trong password_logins đều là "platform admin".
-- Hiện chỉ passwordauth.EnsureBootstrap tạo ra dòng đó (bootstrap
-- admin), nên chấp nhận được. R1 thay giả định này bằng membership trong
-- org platform (orgs.kind = 'platform'): app_platform_role() sẽ JOIN
-- memberships với orgs WHERE kind = 'platform', và hàm chỉ trả về role
-- trong org đó ('admin' khi là platform admin, NULL khi không).

CREATE OR REPLACE FUNCTION app_platform_role() RETURNS TEXT
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public AS $$
  SELECT CASE WHEN EXISTS (
    SELECT 1 FROM password_logins
    WHERE user_id = NULLIF(current_setting('app.user_id', true), '')::uuid
  ) THEN 'admin' ELSE NULL END
$$;

REVOKE ALL ON FUNCTION app_platform_role() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app_platform_role() TO portal_app;

-- === 1b. Hàm DEFINER: app_org_role() =================================
-- Role của app.user_id trong org đang thao tác (app.org_id), hoặc NULL.
-- Plan §4.2. SECURITY DEFINER để tránh đệ quy policy (memberships cũng
-- có RLS): hàm chạy với quyền owner (table owner) nên bypass RLS khi
-- đọc memberships. Không nhận tham số tự do làm điều kiện chéo org:
-- chỉ đọc app.user_id và app.org_id (GUC).
--
-- Policy gọi dạng (SELECT app_org_role()) để Postgres tính một lần mỗi
-- câu lệnh (initplan) thay vì một lần mỗi dòng.

CREATE OR REPLACE FUNCTION app_org_role() RETURNS TEXT
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public AS $$
  SELECT m.role FROM memberships m
  WHERE m.org_id  = NULLIF(current_setting('app.org_id', true), '')::uuid
    AND m.user_id = NULLIF(current_setting('app.user_id', true), '')::uuid
$$;

REVOKE ALL ON FUNCTION app_org_role() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app_org_role() TO portal_app;

-- === 2. orgs: tách policy + trigger chặn sửa plan_id (vá L1) =========
-- Policy cũ: orgs_tenant FOR ALL USING (id = ORG OR EXISTS membership).
-- WITH CHECK ngầm bằng USING -> member có thể UPDATE plan_id.

DROP POLICY IF EXISTS orgs_tenant ON orgs;

-- SELECT: id = ORG, hoặc là thành viên (memberships có RLS, EXISTS
-- không đệ quy vì memberships SELECT có nhánh user_id = me).
CREATE POLICY orgs_select ON orgs FOR SELECT
    USING (
        id = NULLIF(current_setting('app.org_id', true), '')::uuid
        OR EXISTS (
            SELECT 1 FROM memberships m
            WHERE m.org_id = orgs.id
              AND m.user_id = NULLIF(current_setting('app.user_id', true), '')::uuid
        )
    );

-- INSERT: id = ORG (BootstrapTx tạo org mới). R0 không cho platform
-- admin INSERT org qua portal_app (R1+).
CREATE POLICY orgs_insert ON orgs FOR INSERT
    WITH CHECK (id = NULLIF(current_setting('app.org_id', true), '')::uuid);

-- UPDATE: id = ORG VÀ caller là owner/admin của org đó (plan §5.1).
-- Review fix: nhánh "là thành viên" cũ cho member đổi tên/slug của org.
-- app_org_role() đọc membership của app.user_id trong app.org_id;
-- gọi dạng (SELECT app_org_role()) thành initplan (một lần mỗi câu,
-- không phải mỗi dòng). Trigger vẫn chặn sửa plan_id.
CREATE POLICY orgs_update ON orgs FOR UPDATE
    USING (
        id = NULLIF(current_setting('app.org_id', true), '')::uuid
        AND (SELECT app_org_role()) IN ('owner', 'admin')
    )
    WITH CHECK (
        id = NULLIF(current_setting('app.org_id', true), '')::uuid
        AND (SELECT app_org_role()) IN ('owner', 'admin')
    );

-- DELETE: R0 chặn qua portal_app (org không tự xóa). Login migration
-- (table owner) vẫn xóa được vì bypass RLS. R1+ sẽ mở cho platform owner.
CREATE POLICY orgs_delete ON orgs FOR DELETE
    USING (false);

-- Trigger chặn sửa plan_id qua portal_app. Cột kind/status sẽ được
-- thêm ở R1; tạm thời chỉ kiểm plan_id. Khi không có app.user_id
-- (login migration), không chặn.
CREATE OR REPLACE FUNCTION orgs_guard_trigger() RETURNS TRIGGER
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
BEGIN
  -- Login migration (không có app.user_id) đi qua; chỉ chặn portal_app.
  IF NULLIF(current_setting('app.user_id', true), '') IS NULL THEN
    RETURN NEW;
  END IF;
  IF NEW.plan_id IS DISTINCT FROM OLD.plan_id
     AND (SELECT app_platform_role()) IS NULL THEN
    RAISE EXCEPTION 'insufficient_privilege: orgs.plan_id is platform-managed'
      USING ERRCODE = '42501';
  END IF;
  RETURN NEW;
END;
$$;

REVOKE ALL ON FUNCTION orgs_guard_trigger() FROM PUBLIC;
-- Trigger function không cần GRANT EXECUTE: trigger chạy với DEFINER
-- quyền, không qua portal_app gọi trực tiếp.

DROP TRIGGER IF EXISTS orgs_guard ON orgs;
CREATE TRIGGER orgs_guard BEFORE UPDATE ON orgs
  FOR EACH ROW EXECUTE FUNCTION orgs_guard_trigger();

-- === 3. memberships: tách policy (vá L2, L3) =========================
-- Policy cũ: memberships_tenant FOR ALL USING (user_id = me OR org_id =
-- ORG). WITH CHECK ngầm -> tự UPDATE role='owner' (L2), tự INSERT vào
-- org khác (L3). accept invite đi qua DEFINER (mục 4), không qua policy
-- INSERT này.

DROP POLICY IF EXISTS memberships_tenant ON memberships;

CREATE POLICY memberships_select ON memberships FOR SELECT
    USING (
        user_id = NULLIF(current_setting('app.user_id', true), '')::uuid
        OR org_id = NULLIF(current_setting('app.org_id', true), '')::uuid
    );

-- INSERT: chỉ BootstrapTx (org mới, role = owner, chưa có thành viên).
-- accept_invite DEFINER bypass RLS; mọi INSERT khác phải qua hàm đó.
-- L3 cố gắng INSERT (B, me, 'owner') với app.org_id rỗng: org_id = B
-- khác NULL -> WITH CHECK fail.
CREATE POLICY memberships_insert ON memberships FOR INSERT
    WITH CHECK (
        org_id = NULLIF(current_setting('app.org_id', true), '')::uuid
        AND role = 'owner'
        AND NOT EXISTS (
            SELECT 1 FROM memberships m
            WHERE m.org_id = NULLIF(current_setting('app.org_id', true), '')::uuid
        )
    );

-- UPDATE: admin/owner đổi role thành viên khác (user_id <> ME); role
-- mới = 'owner' chỉ khi caller là owner của org. L2 cố gắng UPDATE
-- role='owner' WHERE user_id=me: user_id <> ME fail -> 0 dòng hoặc 42501.
-- Review fix: USING phải cũng yêu cầu R IN (owner, admin), không chỉ
-- org_id = ORG, nếu không member có thể hạ/chỉnh người khác.
-- role cũ/mới = 'owner' chỉ khi R = 'owner' (cho phép owner grant
-- owner, chặn admin grant owner).
CREATE POLICY memberships_update ON memberships FOR UPDATE
    USING (
        org_id = NULLIF(current_setting('app.org_id', true), '')::uuid
        AND (SELECT app_org_role()) IN ('owner', 'admin')
        AND user_id <> NULLIF(current_setting('app.user_id', true), '')::uuid
        AND (role <> 'owner' OR (SELECT app_org_role()) = 'owner')
    )
    WITH CHECK (
        org_id = NULLIF(current_setting('app.org_id', true), '')::uuid
        AND (SELECT app_org_role()) IN ('owner', 'admin')
        AND user_id <> NULLIF(current_setting('app.user_id', true), '')::uuid
        AND (role <> 'owner' OR (SELECT app_org_role()) = 'owner')
    );

-- DELETE: owner/admin gỡ thành viên khác (user_id <> ME, role <> owner);
-- hoặc member tự rời org (user_id = ME, role <> owner). Review fix:
-- chính sách cũ chỉ yêu cầu org_id = ORG, cho phép member đá owner.
CREATE POLICY memberships_delete ON memberships FOR DELETE
    USING (
        org_id = NULLIF(current_setting('app.org_id', true), '')::uuid
        AND (
            ((SELECT app_org_role()) IN ('owner', 'admin')
             AND user_id <> NULLIF(current_setting('app.user_id', true), '')::uuid
             AND role <> 'owner')
            OR (user_id = NULLIF(current_setting('app.user_id', true), '')::uuid
                AND role <> 'owner')
        )
    );

-- === 4. accept_invite SECURITY DEFINER (vá L2, L3 đường invite) =====
-- Hàm kiểm tra token hash, hạn, email; INSERT membership. SECURITY
-- DEFINER (owner = table owner) bypass RLS, nên INSERT trong hàm không
-- qua memberships_insert policy. Loại bỏ đường INSERT membership chỉ
-- dựa vào user_id = me.
--
-- Trả lỗi qua OUT parameter thay vì RAISE EXCEPTION: raise sẽ abort
-- transaction bên ngoài, làm callback không thể commit dù Go đã bắt
-- lỗi. OUT (o_role, o_error) trả về NULL/NULL khi thành công, hoặc
-- (NULL, '<code>') khi lỗi, transaction vẫn dùng được.
--
-- Error codes (o_error):
--   not_found      = token sai, hết hạn, hoặc đã nhận
--   email_mismatch = email khác invite
--   already_member = unique violation khi INSERT membership

CREATE OR REPLACE FUNCTION accept_invite(
  p_org_id UUID,
  p_token_hash TEXT,
  p_user_id UUID,
  p_email CITEXT,
  OUT o_role TEXT,
  OUT o_error TEXT
) LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE
  v_role TEXT;
  v_invite_email CITEXT;
BEGIN
  SELECT role, email INTO v_role, v_invite_email
  FROM invites
  WHERE org_id = p_org_id
    AND token_hash = p_token_hash
    AND accepted_at IS NULL
    AND expires_at > now()
  FOR UPDATE;
  IF NOT FOUND THEN
    o_role := NULL;
    o_error := 'not_found';
    RETURN;
  END IF;
  IF v_invite_email IS DISTINCT FROM p_email THEN
    o_role := NULL;
    o_error := 'email_mismatch';
    RETURN;
  END IF;
  -- Cả UPDATE invite và INSERT membership trong cùng subtransaction:
  -- nếu INSERT fail (unique violation), cả hai rollback.
  BEGIN
    UPDATE invites SET accepted_at = now()
    WHERE org_id = p_org_id AND token_hash = p_token_hash;
    INSERT INTO memberships (org_id, user_id, role)
    VALUES (p_org_id, p_user_id, v_role);
  EXCEPTION WHEN unique_violation THEN
    o_role := NULL;
    o_error := 'already_member';
    RETURN;
  END;
  o_role := v_role;
  o_error := NULL;
  RETURN;
END;
$$;

REVOKE ALL ON FUNCTION accept_invite(UUID, TEXT, UUID, CITEXT) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION accept_invite(UUID, TEXT, UUID, CITEXT) TO portal_app;

-- === 5. model_connections: app_platform_role() để ghi nền tảng (vá L4)
-- Policy 0009: ghi dòng org_id NULL khi app.org_id rỗng. Mọi tx WithTx
-- đều có app.org_id rỗng -> bất kỳ user nào cũng ghi được platform
-- connection. R0 thêm điều kiện (SELECT app_platform_role()) IS NOT
-- NULL (bản tạm: password_logins = bootstrap admin). R1 sẽ thay bằng
-- org platform admin.

DROP POLICY IF EXISTS model_connections_tenant ON model_connections;
DROP POLICY IF EXISTS model_connections_read ON model_connections;
DROP POLICY IF EXISTS model_connections_insert ON model_connections;
DROP POLICY IF EXISTS model_connections_update ON model_connections;
DROP POLICY IF EXISTS model_connections_delete ON model_connections;

-- SELECT: như cũ (mọi org đọc metadata platform, không bao giờ đọc key
-- qua API).
CREATE POLICY model_connections_read ON model_connections FOR SELECT
    USING (
        org_id IS NULL
        OR org_id = NULLIF(current_setting('app.org_id', true), '')::uuid
    );

-- INSERT/UPDATE/DELETE dòng org_id NULL: cần app.org_id rỗng VÀ là
-- platform admin (R0: bootstrap admin). Dòng org_id NOT NULL: org đó.
CREATE POLICY model_connections_insert ON model_connections FOR INSERT
    WITH CHECK (
        (org_id IS NULL
         AND NULLIF(current_setting('app.org_id', true), '') IS NULL
         AND (SELECT app_platform_role()) IS NOT NULL)
        OR org_id = NULLIF(current_setting('app.org_id', true), '')::uuid
    );

CREATE POLICY model_connections_update ON model_connections FOR UPDATE
    USING (
        (org_id IS NULL
         AND NULLIF(current_setting('app.org_id', true), '') IS NULL
         AND (SELECT app_platform_role()) IS NOT NULL)
        OR org_id = NULLIF(current_setting('app.org_id', true), '')::uuid
    )
    WITH CHECK (
        (org_id IS NULL
         AND NULLIF(current_setting('app.org_id', true), '') IS NULL
         AND (SELECT app_platform_role()) IS NOT NULL)
        OR org_id = NULLIF(current_setting('app.org_id', true), '')::uuid
    );

CREATE POLICY model_connections_delete ON model_connections FOR DELETE
    USING (
        (org_id IS NULL
         AND NULLIF(current_setting('app.org_id', true), '') IS NULL
         AND (SELECT app_platform_role()) IS NOT NULL)
        OR org_id = NULLIF(current_setting('app.org_id', true), '')::uuid
    );
