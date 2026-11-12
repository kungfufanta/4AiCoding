# KẾ HOẠCH — Coding agent: agent ở Portal, làm việc trong sandbox

> Trạng thái: **bản plan, chưa có code**. Viết ngày 2026-11-11 để z.ai (hoặc Claude) làm theo từng phase.
> Liên quan: `PLAN.md` (control plane, mục 8 bảo mật), `docs/PORTAL_PLAN.md` (S0–S6), `docs/PORTAL_PLAN_V2.md` (P1–P6).

## 0. Tóm tắt

- Người dùng (hoặc một hệ multi-agent bên ngoài) giao việc kiểu *"thêm trang login cho app này"* trên Portal. **Vòng lặp agent (LLM ↔ tool) chạy ở Portal**, không chạy trong sandbox.
- Agent "làm tay chân" trong sandbox qua **gateway Portal → sandboxd**: đọc/ghi file, chạy lệnh, xem preview, git checkpoint. Sandbox chỉ là máy để agent thao tác, không chứa agent, không chứa key LLM.
- Học lại từ sandboxd gốc (`tastyeffectco/sandboxd`): mô hình task + event stream, checkpoint git + revert, kết quả trung thực (`files_changed`, `build_status`, `preview_ok`), system prompt, cấu hình "model connection". **Không** mang lại `runtimed`, các CLI agent trong sandbox (OpenCode/Claude Code/Codex/Pi) và auth proxy.
- **Template + skill + hồ sơ khách** (mục 5.8, phase T1–T3): khách chọn loại web, điền thông tin doanh nghiệp trước; hệ thống dựng sẵn site từ template, agent nhận hồ sơ như tham số và dùng skill của template.
- **LLM free mặc định: WebHub** (`glm-4.6`, OpenAI-compatible, có memory theo key/user/session). Mỗi org một key `wh_` riêng; memory tắt trong vòng lặp tool (mục 5.2.1).
- Các phase A0–A6 và T1–T3, mỗi phase một PR. A0 làm được ngay (chỉ đụng `control-plane/`); A3 trở đi cần P4 (gateway) của Portal.

---

## 1. Sandboxd gốc làm agent thế nào (review upstream)

Review ở commit `c14612f` (2026-11-11) của `tastyeffectco/sandboxd`. Repo của mình fork từ `92f4ffd` và đã xoá các phần dưới đây ở M1/M2.

### 1.1 Kiến trúc upstream

```
console (React) ──/v1──▶ sandboxd (control plane, host)
                           │  POST /v1/sandboxes/{id}/tasks
                           │  HTTP/1.1 qua Unix socket trên workspace mount
                           ▼
                 runtimed (PID chính trong container, dưới tini)
                           │  spawn CLI headless: opencode --auto | claude --dangerously-skip-permissions | codex exec | pi
                           │  LLM base URL = http://sandboxd:9100/<agent>/<upstream>, key giả
                           ▼
                 authproxy (control plane) ── chèn key thật ──▶ Anthropic / OpenAI / Zen / MiniMax / LiteLLM…
```

| Thành phần | Vị trí upstream | Vai trò |
|---|---|---|
| `runtimed` | `cmd/runtimed` (~6.9k dòng) | Supervisor trong sandbox: chạy dev server theo `sandbox.yaml`, probe health, **chạy task agent** (1 task/lần, `409 task_in_progress`) |
| Adapter agent | `cmd/runtimed/{opencode,claude,codex,pi}.go` | Gọi CLI ở chế độ headless, parse stream của từng CLI thành event chuẩn `message`/`tool` |
| Giao thức | `internal/runtime` | `StartTaskRequest`, `Event{id,type,ts,data}`, `TaskResult`; event `status`/`message`/`tool`/`build`/`run_settings`/`done` (`done` là event kết thúc duy nhất) |
| Lưu event | `.runtimed/tasks/<id>/events.jsonl` | Stream NDJSON, resume bằng `?since=`; task bị ngắt giữa chừng → `failed`, **không bao giờ resume** |
| Checkpoint | `cmd/runtimed/workspace.go` | Trước mỗi task: commit git vào ref riêng `refs/sandboxd/checkpoints/<task>` (commit-tree + index riêng, không đụng HEAD). Dùng làm mốc `files_changed` và để `POST .../tasks/{taskId}/revert` |
| Kết quả task | `runtime.TaskResult` | `files_changed` (từ git diff), `build_status` (passed/failed/skipped), `preview_ok`, `app_healthy` |
| Auth proxy | `internal/authproxy`, `internal/agentauth` | Key/OAuth token ở control plane, chèn header trên đường đi → **không credential nào vào sandbox** |
| Model connection | `internal/modelprovider`, `internal/modelconfig` | Kết nối do operator cấu hình: `protocol` = `openai-completions` / `anthropic-messages`, `base_url`, `api_key` (AES-GCM, write-only), danh sách model + `context_window`/`max_tokens`, nút **Test model** |
| System prompt | `internal/agentprompt/prompt.md` | Dạy agent về môi trường: bind `0.0.0.0:{{PORT}}`, đừng phá `sandbox.yaml`, tự kiểm tra bằng `curl {{LOCAL_URL}}`, đọc `AGENTS.md`, ghi `BRAIN.md` (trí nhớ dự án, không commit) |
| Files/Git API | `internal/api/v1_files*.go`, `/v1/apps/{id}/git/*` | Cây file, đọc/ghi (chặn path traversal + symlink), git status/diff/commit (trong sandbox), push (ở host, PAT không vào sandbox) |
| Operator agent | `docs/operator-agents.md` (Hermes) | Agent chạy *cạnh* app, chỉ có 4 tool MCP (`list_apps`, `create_app`, `ask_app`, `app_status`), giới hạn 10 app/ngày, input coi là dữ liệu không đáng tin |
| UI | `console/` | Chat agent, lịch sử task + revert, editor file, diff/commit/push |

### 1.2 Điểm hay nên giữ (ý tưởng, có thể cả code)

1. **Hợp đồng event** đơn giản, có `id` tăng dần, resume được, đúng một event kết thúc.
2. **Checkpoint git ở ref riêng** → revert theo từng task mà không phá lịch sử git của người dùng.
3. **Kết quả trung thực**: `files_changed` tính từ git chứ không tin lời agent; build check và preview check sau task.
4. **Credential không bao giờ vào sandbox.**
5. **Model connection trung lập nhà cung cấp** (OpenAI-compatible + Anthropic-compatible là đủ phủ LiteLLM, Ollama, vLLM, OpenRouter, MiniMax, Anthropic…).
6. **Giới hạn bằng code, không bằng lời nhắc** (Hermes: chỉ vài tool, có quota/ngày).
7. Task bị ngắt → `failed`, không resume mập mờ.

### 1.3 Điểm không hợp với mình

| Upstream | Vì sao không hợp |
|---|---|
| Agent chạy **trong** sandbox (`runtimed` + CLI) | Doan chốt: agent ở Portal gọi vào sandbox. Sandbox của mình chạy `sleep infinity`, image theo từng sandbox (allowlist), không có supervisor |
| Phụ thuộc 4 CLI bên thứ ba + phiên bản ghim trong base image | Khó kiểm soát tool, khó đếm token/chi phí, khó áp quota per-tenant, khó audit từng tool call |
| Auth proxy cần sandbox gọi ra control plane | Sandbox phải mở egress tới proxy; mình đang có 3 chế độ egress + blocklist, thêm đường ra là thêm rủi ro |
| Claude subscription login trong sandbox, import OAuth của CLI | Rủi ro điều khoản dịch vụ, không hợp SaaS nhiều tenant |
| Single-tenant, SQLite, một host | Mình có Portal nhiều org (Postgres + RLS), worker trên k3s |

---

## 2. Hiện trạng repo mình (2026-11-11)

**sandboxd (`control-plane/`)** — đủ cho lifecycle, thiếu cho agent:

| Có | Thiếu cho agent |
|---|---|
| `POST /v1/sandboxes/{id}/exec` `{cmd: string[]}` → `{stdout, stderr, exit_code}` | **Không có timeout, `cwd`, `env`, `stdin`, giới hạn output** (`docker.Exec` buffer toàn bộ output vào RAM). Lệnh treo = request treo |
| `GET /v1/sandboxes/{id}/logs?follow=true` (SSE) | — |
| Snapshot, egress 3 chế độ + blocklist, ports, approvals, audit hash chain, API key scope `sandboxes:exec` | **Không có files API** (đã xoá ở M1). Đọc/ghi file chỉ có cách `exec cat`/`exec sh -c 'cat > f'` → lỗi quoting, file nhị phân, file lớn |
| `gitimport` clone repo public, image có `git`, `ripgrep`, `fd`, `jq` (base image) | Image khác trong allowlist có thể không có `git`/`rg` |
| Runtime k8s (PR #10, chưa merge) | Exec trên k8s phải đi qua `pods/exec` (SPDY/WebSocket) |

**Portal (`portal/`)** — đã có S0–S2 (P1, P2): OIDC Zitadel, org/project + RLS, API key `sbx_live_…`, agent token EdDSA (`POST /v1/tokens`, claim có sẵn `agent_run_id`). Chưa có: P3 (tag tenant ở sandboxd), **P4 (gateway + trang Sandboxes)**, P5 (quota/usage), P6 (SDK + MCP).

→ Agent cần P4. Plan này **chen sau P4**, dùng lại gateway, và đẩy một phần việc ở sandboxd (A0) làm song song ngay.

---

## 3. Kiến trúc đích

```
Người dùng (Portal UI)          Hệ multi-agent ngoài (API key / agent token / MCP)
        │  BFF cookie                    │ Bearer
        ▼                                ▼
┌─────────────────────── Portal (k3s, ns 4aicoding) ───────────────────────┐
│  portal-api  ── POST /v1/sandboxes/{id}/agent/runs ──▶ agent_runs (PG)   │
│      ▲  SSE /agent/runs/{id}/events  ◀── LISTEN/NOTIFY ── agent_events   │
│      │                                                                   │
│  portal-agent-worker (Deployment, N replica, cùng image, subcommand)     │
│      claim run (FOR UPDATE SKIP LOCKED + lease) → vòng lặp:              │
│        LLM client ──HTTPS──▶ model connection của org (key giải mã RAM)  │
│        tool call  ──▶ gateway nội bộ (kiểm tra sở hữu sandbox)           │
└──────────────────────────────────────────┬───────────────────────────────┘
                                           │ service key sandboxd (mạng nội bộ)
                                           ▼
              sandboxd: exec v2 (timeout, cwd, giới hạn output) + files API
                                           │ docker exec / k8s pods/exec
                                           ▼
                      sandbox (zero trust: không key, không agent, egress theo policy)
```

Nguyên tắc:

1. **Agent là một client của API, không có đặc quyền** (PLAN.md §0). Mọi tool đi qua gateway Portal → sandboxd như API thường, có audit.
2. **Run gắn cứng với một `sandbox_id` + `org_id`.** Worker chỉ được gọi tool lên đúng sandbox đó; kiểm tra ở code gateway, không dựa vào prompt.
3. **Key LLM chỉ ở Portal**, mã hoá trong Postgres, chỉ giải mã trong RAM của worker khi gọi LLM. Sandbox không cần egress tới nhà cung cấp LLM.
4. **Mọi thứ đọc từ sandbox/repo/web là dữ liệu**, không phải lệnh (chống prompt injection): giới hạn bằng tool whitelist, quota, timeout, chứ không bằng lời nhắc.
5. **Durable**: trạng thái run và event nằm trong Postgres → nhiều replica API đọc được, worker chết thì run được kết thúc rõ ràng.

---

## 4. Mang lại gì, viết lại gì

| Hạng mục | Quyết định | Ghi chú |
|---|---|---|
| Files API (`v1_files.go`, `v1_files_write.go`, `safeJoin`, kiểm tra symlink) | **Mang lại** vào sandboxd (A0), chỉnh cho path gốc `/home/sandbox` | Có test upstream đi kèm, port cả test |
| Giao thức task/event (`internal/runtime/tasks.go`) | **Mang lại ý tưởng**, viết lại thành schema Postgres + JSON ở Portal | Giữ tên event: `status`, `message`, `tool_call`, `tool_result`, `build`, `done` |
| Checkpoint git ref riêng (`workspace.go`) | **Port logic**, chạy qua exec từ Portal thay vì trong runtimed | Lệnh git cụ thể ở mục 5.4 |
| `TaskResult` (`files_changed`, `build_status`, `preview_ok`) | **Giữ** | |
| `agentprompt/prompt.md` | **Mang lại, sửa** cho mô hình mới (không còn runtimed/sandbox.yaml; giữ `AGENTS.md`, `BRAIN.md`, bind `0.0.0.0`) | |
| `modelconfig` + `modelprovider` (schema connection, probe/test) | **Port vào Portal**, scope theo org, RLS | Thêm connection mặc định cấp nền tảng |
| `authproxy`, `agentauth`, CLI adapter, `runtimed`, Claude login | **Không mang lại** | Có thể xem lại ở A7 (tuỳ chọn) |
| Operator agent Hermes | **Không mang lại** | Ý tưởng "ít tool + quota/ngày" được dùng cho MCP `run_agent_task` |
| Vòng lặp agent | **Viết mới** bằng Go trong `portal/internal/agent` | Không dùng framework ngoài; hai protocol LLM viết tay, có streaming + tool use |

---

## 5. Thiết kế chi tiết

### 5.1 Thay đổi ở sandboxd (A0)

**Exec v2** — mở rộng `POST /v1/sandboxes/{id}/exec`, tương thích ngược:

```json
{ "cmd": ["bash","-lc","pnpm test"], "cwd": "/home/sandbox/workspace", "env": {"CI":"1"},
  "timeout_s": 120, "max_output_bytes": 262144, "stdin": "…(tuỳ chọn, ≤1 MiB)" }
→ { "stdout": "…", "stderr": "…", "exit_code": 0, "timed_out": false,
    "truncated": false, "duration_ms": 1834 }
```

- `timeout_s` mặc định 60, tối đa theo `policy.exec.max_timeout_s` (mặc định 600). Hết giờ: kill cả process group trong container (`docker exec` chạy qua `timeout --signal=KILL` hoặc `setsid` + kill), trả `timed_out: true`, `exit_code: 124`.
- `max_output_bytes` mặc định 256 KiB, tối đa 4 MiB; vượt thì giữ đầu + cuối, `truncated: true`. Không buffer vô hạn trong RAM control plane nữa.
- `env` không được ghi đè `PATH`/`HOME`/`LD_*`; `cwd` phải nằm dưới `/home/sandbox`.
- Thêm `?stream=ndjson` cho lệnh dài: mỗi dòng `{"stream":"stdout","data":"…"}` …, dòng cuối `{"exit_code":…}`.
- Áp dụng cho cả runtime docker và runtime k8s (PR #10: `pods/exec`).

**Files API** — scope mới `sandboxes:files` (mặc định cấp cho role `operator`):

| Route | Ghi chú |
|---|---|
| `GET /v1/sandboxes/{id}/files?path=&recursive=&max_entries=` | Liệt kê; bỏ qua `node_modules`, `.git`, `.venv` khi recursive |
| `GET /v1/sandboxes/{id}/files/content?path=&offset=&limit=` | Đọc text (UTF-8) hoặc base64 khi nhị phân; tối đa 2 MiB/lần |
| `PUT /v1/sandboxes/{id}/files/content?path=` | Ghi nguyên file (tạo thư mục cha), tuỳ chọn `if_match` (sha256) chống ghi đè |
| `DELETE /v1/sandboxes/{id}/files?path=` | Xoá file/thư mục rỗng |
| `POST /v1/sandboxes/{id}/files/move` | `{from,to}` |

- Làm trên workspace mount ở host (như upstream) khi sandbox chạy local; với worker từ xa/k8s thì forward tới executor worker sở hữu sandbox (cơ chế `forwardRemote` ở `internal/api/fleet_exec.go` đã có từ M7b); với runtime k8s (PR #10) thì dùng exec + tar.
- Path gốc là `/home/sandbox`; chặn `..`, symlink trỏ ra ngoài (resolve rồi kiểm tra lại, như upstream `v1FileContent`), chặn `/home/sandbox/.ssh` nếu có. Ghi file đặt owner uid 1000.
- File ghi qua API không chạy được bằng quyền host (không `chmod +x` ở host; chmod qua exec).
- Audit: `sandbox.file.write`/`delete` ghi path, không ghi nội dung.

### 5.2 Model connections ở Portal (A2)

```sql
model_connections(id uuid, org_id uuid NULL /* NULL = cấp nền tảng */, name text,
  protocol text CHECK (protocol IN ('anthropic-messages','openai-chat')),
  flavor text NOT NULL DEFAULT 'generic' CHECK (flavor IN ('generic','webhub')),  -- mục 5.2.1
  key_mode text NOT NULL DEFAULT 'shared' CHECK (key_mode IN ('shared','per_org','per_project')),
  base_url text, api_key_enc bytea NULL /* key_mode = shared */,
  models jsonb /* [{id,context_window,max_output,supports_tools,input_price,output_price}] */,
  default_model text, created_by uuid, created_at, updated_at, disabled_at)

-- key riêng của từng tenant cho connection key_mode = per_org / per_project
model_connection_keys(connection_id, org_id, project_id NULL, api_key_enc bytea, key_label text,
  created_at, rotated_at, PRIMARY KEY (connection_id, org_id, project_id))
```

- RLS theo `org_id`; dòng `org_id IS NULL` là connection nền tảng do platform admin tạo, mọi org đọc được metadata (không bao giờ đọc được key).
- Key mã hoá AES-256-GCM, khoá từ Secret k8s `PORTAL_SECRETS_KEY` (có `kid` để xoay vòng). API chỉ trả `has_api_key`.
- `base_url` chỉ cho `https://` (trừ allowlist nội bộ do admin đặt, ví dụ `http://litellm.4aicoding.svc:4000`). Chặn IP private/link-local khi org tự nhập (chống SSRF vào mạng cluster).
- `POST /api/orgs/{org}/model-connections/{id}/test` gửi một request nhỏ (như upstream).
- Role: chỉ `owner`/`admin` của org tạo/sửa; `member` chọn model khi chạy.

#### 5.2.1 Connection mặc định (free): WebHub

Doan chốt (2026-11-11): agent **free mặc định** dùng WebHub tại `https://h11hp9dd1y90-d.space-z.ai`. Đây là hub OpenAI-compatible (`POST /v1/chat/completions`, SSE chuẩn `data: {...}` + `[DONE]`, `GET /v1/models`), model mặc định `glm-4.6`. Nó khác API thường ở chỗ có **bộ nhớ dài hạn built-in** và **mô hình tenant 3 tầng**:

| Tầng WebHub | Cách truyền | Ranh giới |
|---|---|---|
| Key `wh_…` | `Authorization: Bearer wh_…` | Tenant. Memory, session, request log của key này không bao giờ lẫn sang key khác |
| User | field `user` (chuẩn OpenAI) hoặc header `X-WebHub-User` | End-user trong tenant; chủ của bộ nhớ xuyên phiên (`scope=user`) |
| Session | header `X-Session-Id` | Một cuộc trò chuyện; không có user thì "ghi nhớ" chỉ sống trong session |

Thêm: memory `scope=client` là **kiến thức dùng chung cho mọi user của cùng key**; tắt memory theo request bằng `X-WebHub-Memory: 0` hoặc body `"memory": false`; `X-Request-Id` cho retry idempotent; header phản hồi `X-WebHub-Memory-Items`, `X-WebHub-Session`, `X-WebHub-Request-Id`; REST `POST/GET /api/memory`, `POST /api/memory/search`, `DELETE /api/memory/{id}`.

**Ánh xạ tenant của Portal sang WebHub**

| Portal | WebHub | Lý do |
|---|---|---|
| **Org** (hoặc project, xem Q14) | **một key `wh_` riêng** (`key_mode = per_org`) | Bắt buộc. Nếu nhiều org dùng chung một key thì memory `scope=client`, session và request log của org này đọc được từ org kia. Không bao giờ dùng chung một key `wh_` giữa các org |
| User Portal (UUID) | `X-WebHub-User: u_<user_uuid>` | Dùng UUID, không dùng email (không gửi PII ra ngoài) |
| API key / agent token (hệ multi-agent ngoài) | `X-WebHub-User: k_<api_key_id>` | Mỗi key là một "end-user" riêng, không đọc memory của người dùng |
| `agent_sessions.id` | `X-Session-Id: s_<session_uuid>` | Gom các run của cùng một cuộc chat |
| `run_id` + số bước | `X-Request-Id: <run_id>:<step>` | Worker retry không bị tính hai lần |

Header tenant do **worker gắn từ dữ liệu của run** (org/user/session lấy từ DB), không bao giờ lấy từ prompt hay tool output. Test bắt buộc: run của org A không thể phát ra request mang key của org B.

**Cách dùng memory cho coding agent (đề xuất)**

Memory tự động của hub hữu ích cho chat, nhưng **nguy hiểm trong vòng lặp tool**: hub tự trích "ký ức" từ hội thoại, mà hội thoại của agent chứa nội dung file, output lệnh, có thể có `.env`. Nội dung đó có thể (1) bị lưu lâu dài bên ngoài, (2) quay lại như "background knowledge" ở các run sau, thành đường prompt injection bền vững. Vì vậy:

- Mọi request **trong vòng lặp tool** gửi `X-WebHub-Memory: 0`.
- Chỉ **lượt LLM đầu tiên của mỗi run** (chưa có tool output) bật memory, để agent nhận sở thích của người dùng ("thích tông xanh, trả lời tiếng Việt").
- Portal **chủ động ghi** memory qua `POST /api/memory`, có kiểm soát: tóm tắt hồ sơ khách (mục 5.8) và `finish.summary` của run thành công (`scope=user`, `kind=summary`), không bao giờ ghi nội dung file hay output lệnh.
- Khối `[Hub memory …]` hub chèn vào được coi là **dữ liệu không đáng tin**, giống tool output.
- Khi org bị xoá: gọi `DELETE /api/memory/{id}` cho mọi item của key, rồi thu hồi key (cần API quản lý key của hub, Q14).
- `BRAIN.md` trong repo vẫn là trí nhớ dự án chính (đọc được, review được, đi theo snapshot); memory của hub chỉ là trí nhớ về **người dùng**.

**Cấu hình**

- Connection nền tảng được seed lúc Portal khởi động từ env: `PORTAL_DEFAULT_LLM_BASE_URL=https://h11hp9dd1y90-d.space-z.ai/v1`, `PORTAL_DEFAULT_LLM_MODEL=glm-4.6`, `PORTAL_DEFAULT_LLM_FLAVOR=webhub`. Key `wh_` thật nằm trong Secret k8s / bảng `model_connection_keys` (mã hoá), **không bao giờ commit vào repo** (kể cả demo key).
- `flavor = webhub` chỉ thêm header (`X-WebHub-User`, `X-Session-Id`, `X-WebHub-Memory`, `X-Request-Id`) và đọc header phản hồi để log; body vẫn là OpenAI chuẩn, nên cùng một client `openaichat` dùng được cho LiteLLM/OpenRouter/vLLM.
- Domain hub được thêm vào NetworkPolicy egress của `portal-agent-worker` (A6).
- **Không** chạy `install.sh` "worker onboarding" của WebHub trong sandbox: đó là cách đăng ký máy làm worker tính toán cho hub, sẽ đưa key của hub vào sandbox và mở egress, trái nguyên tắc "không credential trong sandbox".

**Phải kiểm chứng ở đầu A2 (spike, ghi kết quả vào PR)**

1. `glm-4.6` qua hub có hỗ trợ `tools` / `tool_choice` và trả `tool_calls` dạng delta khi `stream: true` không. Nếu không, phải có chế độ dự phòng (tool call dạng JSON trong text, parse chặt).
2. Context window, max output thực tế, rate limit của gói free.
3. Memory có bị inject khi gửi `X-WebHub-Memory: 0` không (đọc `X-WebHub-Memory-Items` phải bằng 0).
4. Hub có API tạo/thu hồi key `wh_` theo tenant không (để Portal tự cấp key khi tạo org).

### 5.3 Vòng lặp agent (A3)

Package `portal/internal/agent`:

```
llm/            Client interface: Stream(ctx, Request) → iterator[Delta]; hai impl: anthropic, openaichat
tools/          Registry, JSON schema mỗi tool, thực thi qua gateway client
loop.go         Run(ctx, run) : build prompt → gọi LLM → thực thi tool → ghi event → lặp
prompt/         system.md (port từ agentprompt, sửa), nạp AGENTS.md/BRAIN.md của repo
checkpoint.go   git checkpoint / diff / revert qua exec
worker.go       claim run, lease, heartbeat, cancel
```

Vòng lặp:

1. Claim run `queued` (`SELECT … FOR UPDATE SKIP LOCKED`), đặt `lease_until = now()+60s`, gia hạn mỗi 20 s.
2. Đảm bảo sandbox `running` (gọi `start` nếu `stopped`; bật keep-alive trong lúc chạy, tắt khi xong).
3. **Checkpoint** (mục 5.4) → event `status{phase:"checkpoint"}`.
4. Dựng context: system prompt + mô tả workspace (cây thư mục rút gọn, `AGENTS.md`, `BRAIN.md` nếu có) + lịch sử session (nếu `continue`) + prompt của người dùng.
5. Lặp tối đa `max_steps` (mặc định 50): gọi LLM streaming → đẩy `message` delta (gộp 250 ms) → nếu có tool call: kiểm tra schema, thực thi, ghi `tool_call` + `tool_result` (cắt ngắn output > 16 KiB trước khi đưa lại LLM, lưu bản đầy đủ tối đa 256 KiB) → lặp. Dừng khi LLM trả lời không kèm tool, gọi `finish`, hết bước, hết ngân sách token, hết thời gian (mặc định 20 phút), hoặc bị cancel.
6. Hậu kiểm: `files_changed` từ git diff so với checkpoint; `build` nếu repo khai báo (`AGENTS.md` / detect recipe) → `build_status`; nếu sandbox có port public → `preview_ok` bằng exec `curl -fsS -o /dev/null -w '%{http_code}' http://127.0.0.1:<port>/`.
7. Event `done{result}`; cập nhật `agent_runs`; ghi `usage_events` (token vào/ra, chi phí ước tính).

Nén context: khi tổng token > 70 % `context_window`, tóm tắt các bước cũ bằng một lời gọi LLM riêng, giữ nguyên 6 bước cuối.

Bộ tool (cố định, không cho agent tự thêm):

| Tool | Thực thi | Giới hạn |
|---|---|---|
| `list_files(path, depth)` | files API | ≤ 2 000 mục |
| `read_file(path, offset, limit)` | files API | ≤ 2 000 dòng/lần, trả kèm số dòng |
| `write_file(path, content)` | files API | ≤ 1 MiB |
| `edit_file(path, old, new, replace_all)` | đọc → thay chuỗi duy nhất → ghi với `if_match` | lỗi rõ khi `old` không duy nhất |
| `search(pattern, path, glob)` | exec `rg --json` (fallback `grep -rn`) | ≤ 200 kết quả |
| `run_command(command, cwd, timeout_s)` | exec v2 `bash -lc` | timeout ≤ 600 s; output cắt như trên |
| `check_preview(path)` | exec `curl` vào `127.0.0.1:<port>` | |
| `read_skill(name)` | đọc skill của template / nền tảng (mục 5.8) | chỉ skill đã publish |
| `finish(summary)` | kết thúc run | |

Không có tool đổi egress, mở port, xoá sandbox, đọc env/secret của nền tảng. Những việc đó người dùng làm trên UI (và đi qua approvals nếu policy bật).

### 5.4 Checkpoint và revert (port từ upstream)

Chạy qua exec trong sandbox, thư mục làm việc = repo (mặc định `/home/sandbox/workspace`):

```bash
git rev-parse --is-inside-work-tree || git init -q
export GIT_INDEX_FILE=$(mktemp)          # index riêng, không đụng staging của người dùng
git add -A                               # theo .gitignore
tree=$(git write-tree)
parent=$(git rev-parse -q --verify HEAD) && p="-p $parent"
c=$(git -c user.name=4aicoding -c user.email=agent@4aicoding commit-tree $tree $p -m "checkpoint <run_id>")
git update-ref refs/4aicoding/checkpoints/<run_id> $c
```

- `files_changed`: `git diff --name-status refs/4aicoding/checkpoints/<run_id>` (cũng qua index tạm).
- Revert (`POST /v1/agent/runs/{id}/revert`): `git checkout <ref> -- .` + xoá file mới tạo sau checkpoint (`git clean` có giới hạn trên danh sách `files_changed`), event `reverted`. Chỉ revert run mới nhất hoặc chuỗi liên tiếp từ cuối lên (giống upstream) để không trộn trạng thái.
- Image không có `git`: run vẫn chạy nhưng `checkpoint: unavailable`, không revert được; UI hiện cảnh báo. (Câu hỏi mở Q4.)

### 5.5 Dữ liệu ở Portal

```sql
agent_sessions(id, org_id, project_id, sandbox_id, title, model_connection_id, model,
  created_by_user uuid NULL, created_by_key uuid NULL, created_at, archived_at)
agent_runs(id, session_id, org_id, sandbox_id, prompt text, status /* queued|running|succeeded|failed|cancelled */,
  error_code text, checkpoint_ref text, result jsonb, steps int, input_tokens bigint, output_tokens bigint,
  cost_micros bigint, lease_owner text, lease_until timestamptz, cancel_requested bool,
  created_at, started_at, finished_at)
agent_events(run_id, seq int, type text, data jsonb, created_at, PRIMARY KEY(run_id, seq))
agent_messages(session_id, idx, role, content jsonb)   -- lịch sử hội thoại cho "continue"
```

- Tất cả có `org_id` + RLS như `0002_tenants_rls.sql`.
- Mỗi sandbox chỉ có **một run `running`** (unique partial index `WHERE status IN ('queued','running')` theo `sandbox_id`) → request thứ hai `409 run_in_progress` (như upstream).
- Worker mất lease (pod chết) → một worker khác thấy `lease_until < now()` thì kết thúc run `failed / worker_lost`, **không resume** (như upstream). Người dùng bấm "Tiếp tục" để tạo run mới trong cùng session.
- Giữ `agent_events` 30 ngày (job dọn), `agent_runs.result` giữ lâu.

### 5.6 API Portal

| Route | Auth | Ghi chú |
|---|---|---|
| `POST /v1/sandboxes/{id}/agent/runs` | key/token scope `agent:run` | `{prompt, session_id?, model_connection_id?, model?, max_steps?, timeout_s?}`; idempotency key như P4 |
| `GET /v1/sandboxes/{id}/agent/runs` | `agent:read` | phân trang |
| `GET /v1/agent/runs/{runId}` | `agent:read` | |
| `GET /v1/agent/runs/{runId}/events?since=N` | `agent:read` | SSE (`id:` = seq, hỗ trợ `Last-Event-ID`); không follow thì trả JSON |
| `POST /v1/agent/runs/{runId}/cancel` | `agent:run` | đặt `cancel_requested`, worker dừng trong ≤ 5 s |
| `POST /v1/agent/runs/{runId}/revert` | `agent:run` | |
| `GET /v1/agent/sessions?sandbox_id=` | `agent:read` | |
| BFF `/api/orgs/{org}/…` | cookie | cùng logic cho UI |

- Fan-out SSE qua `LISTEN agent_events_<run>` / `NOTIFY` để mọi replica API stream được.
- Agent token có claim `agent_run_id` (đã có ở S2): token xin cho một run chỉ đọc được run đó.
- Webhook (nếu P3 đã có cơ chế): `agent.run.completed` ký HMAC.

### 5.7 UI Portal (A4)

Trang chi tiết sandbox (P4) thêm tab **Agent** và **Files**:

- **Agent**: khung chat, stream message; tool call hiện dạng thẻ thu gọn (tên tool, tham số rút gọn, output, thời gian); cuối run hiện thẻ kết quả: số file đổi (bấm xem diff), build passed/failed, preview ok, token + chi phí. Nút Cancel khi đang chạy, Revert ở run cuối (ConfirmDialog). Chọn model (dropdown theo connection). Danh sách session bên trái. Tự reconnect SSE theo `Last-Event-ID`.
- **Files**: cây thư mục + xem file (highlight nhẹ, lazy-load CodeMirror như upstream), sửa và lưu (dùng `if_match`), tab Diff so với checkpoint của run cuối.
- **Settings → Models** (org admin): bảng connection, Dialog thêm/sửa, nút Test.
- Theo checklist chất lượng UI §4 của `PORTAL_PLAN_V2.md` (dark mode, mobile 390 px, Playwright).

### 5.8 Template, skill và "hồ sơ khách hàng" trước khi vibe code (T1–T3)

Ý tưởng của Doan: trước khi khách vibe code, hệ thống **lọc nhu cầu** (loại web gì), cho khách **nhập trước thông tin doanh nghiệp**, rồi mới giao cho agent. Agent nhận các thông tin đó như tham số (`company_name`, `address`, `phone`…). Ý này **làm được và nên làm**: agent bắt đầu từ một template đã chạy được thay vì trang trắng, nên nhanh hơn, rẻ token hơn, ít lỗi hơn, và kết quả đồng đều. Upstream chỉ có `presets` (5 starter) + `recipes` (nhận diện framework); mình làm rộng hơn thành **thư viện template có skill**.

**Luồng người dùng (wizard "Tạo website")**

1. **Chọn loại web** (lọc mảng KH): Landing giới thiệu công ty · Giới thiệu sản phẩm/dịch vụ · Shop nhỏ (ít sản phẩm, quản lý đơn đơn giản) · Portfolio/cá nhân · Nhà hàng/menu · Blog… Mỗi loại map tới 1–3 template.
2. **Chọn template** (ảnh xem trước, stack hiển thị: Next.js / React+Vite / Astro…).
3. **Điền hồ sơ**: form sinh tự động từ schema của template (tên công ty, slogan, địa chỉ, SĐT, email, giờ mở cửa, link mạng xã hội, logo, màu chủ đạo, danh sách sản phẩm…). Lưu nháp được, sửa lại sau.
4. **Tạo**: Portal tạo sandbox từ template → **điền tham số một cách tất định (không dùng LLM)** → preview chạy ngay → mở tab Agent để khách "vibe" tiếp ("đổi sang tông xanh", "thêm trang tuyển dụng").

**Template = repo + manifest**

```
templates/landing-company-nextjs/
  template.yaml          # metadata + schema tham số + lệnh chạy
  site.config.json       # FILE DUY NHẤT chứa dữ liệu khách; component đọc từ đây
  app/ components/ …     # source Next.js chạy được ngay với dữ liệu mẫu
  public/
  AGENTS.md              # mô tả cấu trúc dự án cho agent
  skills/
    add-section/SKILL.md
    edit-products/SKILL.md
```

```yaml
# template.yaml
id: landing-company-nextjs
version: 1.2.0
category: landing-company          # dùng cho bước lọc
title: "Landing page giới thiệu công ty"
stack: { framework: nextjs, runtime: node22, storage: none }   # none | sqlite
image: base:node22                 # phải nằm trong allowlist image
run: { install: "pnpm install --frozen-lockfile", dev: "pnpm dev -H 0.0.0.0 -p 3000", build: "pnpm build", port: 3000 }
params:                            # JSON Schema rút gọn → form + validate
  company_name: { type: string, required: true, max: 120, label: "Tên công ty" }
  slogan:       { type: string, max: 200 }
  address:      { type: string }
  phone:        { type: string, format: phone }
  email:        { type: string, format: email }
  logo:         { type: file, accept: [image/png, image/svg+xml], max_kb: 512 }
  brand_color:  { type: color, default: "#2563eb" }
  products:     { type: array, max_items: 30, items: { name: string, price: number, image: file, description: string } }
render:
  data_file: site.config.json      # tham số được ghi vào đây
  placeholders: [app/layout.tsx]   # (tuỳ chọn) file có {{ company_name }} cần thay chữ
skills: [add-section, edit-products, change-theme]
```

**Vì sao ghi vào `site.config.json` chứ không thay `${company_name}` khắp source:**

- `${…}` trùng cú pháp template literal của JS/TS, thay chuỗi mù sẽ phá code; khách đổi thông tin sau này phải tìm-thay lại ở nhiều file.
- Dữ liệu ở một file JSON → khách sửa hồ sơ trên Portal thì chỉ cần ghi lại file đó (files API), không cần agent, không tốn token; agent cũng biết chính xác "nguồn sự thật" là file nào.
- Chỉ dùng thay chữ `{{ name }}` (cú pháp riêng, có escape) cho vài chỗ không đọc được JSON (ví dụ `<title>` tĩnh), và chỉ trong các file khai báo ở `render.placeholders`.

**Agent nhận hồ sơ thế nào**

- Portal ghi `.4ai/brief.json` (trong thư mục dự án) (bản sao hồ sơ + loại web + template id/version) vào workspace, và chèn **tóm tắt hồ sơ** vào system prompt: *"Khách: Công ty ABC, địa chỉ …, ngành …; dữ liệu nằm ở `site.config.json`; đừng hardcode thông tin khách vào component"*.
- Thông tin hồ sơ là **dữ liệu người dùng nhập, không phải lệnh** (chống prompt injection: ai đó điền "bỏ qua hướng dẫn trước…" vào ô slogan). Prompt đặt nó trong khối dữ liệu có đánh dấu, có giới hạn độ dài từng field.

**Skill**

- Skill = file Markdown hướng dẫn một việc cụ thể trên template đó ("thêm section", "sửa danh sách sản phẩm", "đổi theme", "thêm trang"), kèm lệnh kiểm tra. Hai nguồn: skill **của template** (`skills/` trong repo template) + skill **dùng chung** của nền tảng (ví dụ "SEO cơ bản", "tối ưu ảnh", "form liên hệ gửi email").
- Agent chỉ nhận **danh sách tên + mô tả 1 dòng** của skill trong prompt; tool mới `read_skill(name)` trả nội dung đầy đủ khi cần (tiết kiệm context, giống cách Claude Code dùng skill).
- Skill được version cùng template; admin nền tảng duyệt trước khi publish (skill là "lệnh" nên phải đáng tin, khác hồ sơ khách).

**Stack theo loại web (đề xuất ban đầu)**

| Loại web | Stack | Lưu trữ |
|---|---|---|
| Landing công ty / giới thiệu sản phẩm / portfolio | Next.js (App Router, static export được) hoặc Astro | không cần, chỉ `site.config.json` |
| Shop nhỏ (≤ ~50 sản phẩm, quản lý đơn đơn giản) | Next.js full-stack (route handler / server action) | SQLite (`better-sqlite3`, file trong workspace) + trang `/admin` có mật khẩu |
| Nhà hàng/menu, blog | Next.js hoặc Astro + Markdown | file Markdown/JSON |

Lưu ý: "không có backend" nghĩa là **không có service backend riêng**; shop có đặt hàng vẫn cần server của Next.js để ghi SQLite. Nếu muốn tĩnh 100 % thì đặt hàng qua Zalo/Messenger/form gửi email (câu hỏi mở Q12). SQLite trong workspace được snapshot cùng sandbox (đã kiểm chứng ở upstream).

**Dữ liệu ở Portal**

```sql
templates(id, slug, version, category, title, stack jsonb, manifest jsonb, source_repo, source_ref,
  preview_image, status /* draft|published|deprecated */, created_at)
site_categories(slug, title, description, sort, template_slugs text[])
customer_briefs(id, org_id, project_id, category, template_id, template_version,
  params jsonb, files jsonb /* logo… lưu object storage */, status /* draft|applied */, created_by, updated_at)
sandboxes.brief_id   -- sandbox tạo từ brief nào
```

- `customer_briefs` có RLS theo org. Đây là **dữ liệu cá nhân/doanh nghiệp** của khách → không log nội dung, có nút xoá.
- Validate `params` bằng schema của đúng version template (server-side), sanitize text (không HTML), giới hạn kích thước file.
- Template lưu ở **một repo riêng** (đề xuất `kungfufanta/4aicoding-templates`, mỗi template một thư mục) — sandbox tạo bằng `gitimport` public sẵn có (clone theo tag `<slug>@<version>`), hoặc đóng sẵn vào image để tạo nhanh không cần mạng (câu hỏi mở Q10).

### 5.9 Bảo mật (bổ sung cho PLAN.md §8)

| Rủi ro | Biện pháp |
|---|---|
| Prompt injection từ repo/web khiến agent phá sandbox khác | Run gắn `sandbox_id` ở code gateway; tool không nhận `sandbox_id` từ LLM |
| Agent tự mở egress/port để exfiltrate | Không có tool cho việc đó; egress theo policy sandbox; mở thêm phải qua UI + approvals |
| Lộ key LLM | Key chỉ ở Portal, giải mã trong RAM; không log request body LLM; test grep log như P2 |
| SSRF qua `base_url` | Chặn IP private/link-local/metadata khi org tự nhập; allowlist nội bộ do platform admin |
| Lệnh treo/đốt tài nguyên | Exec timeout + giới hạn output; `max_steps`, timeout run, ngân sách token theo org (A5) |
| Đốt tiền LLM | Quota token/tháng theo plan, cảnh báo 80 %, chặn 100 % (`429 agent_quota_exceeded`) |
| Worker bị chiếm quyền | Worker dùng service key sandboxd riêng (scope: exec, files, read, start/stop) — không có `sandboxes:egress`, không xoá được sandbox; NetworkPolicy egress chỉ ra sandboxd + Postgres + LLM endpoints |
| Audit | Mỗi tool call ghi `audit_log` Portal (`agent.tool`, tool, path/cmd0, exit code), không ghi nội dung file |

---

## 6. Milestones

Mỗi phase **một PR**, có test, CI xanh. "Xong khi" là tiêu chí nghiệm thu.

### A0 — sandboxd: exec v2 + files API (chỉ `control-plane/`, làm được ngay, song song P3/P4)

- Exec v2 (mục 5.1): `timeout_s`, `cwd`, `env`, `stdin`, `max_output_bytes`, `timed_out`/`truncated`/`duration_ms`, `?stream=ndjson`; policy `exec.max_timeout_s`.
- Files API (mục 5.1), port `v1_files.go` + `v1_files_write.go` + test từ upstream; scope `sandboxes:files`.
- Cập nhật `docs/openapi.yaml`, `scripts/e2e*.sh`.
- **Xong khi:** `sleep 999` với `timeout_s: 2` trả `timed_out:true` trong ≤ 3 s và không còn process trong container; lệnh in 50 MB chỉ tốn RAM control plane ≈ `max_output_bytes`; đọc `../../etc/passwd` và symlink ra ngoài → 400; ghi rồi đọc lại file nhị phân ra đúng sha256; request cũ `{cmd}` vẫn chạy y như trước; key thiếu `sandboxes:files` → 403.

### A1 — Portal gateway cho exec v2 + files (cần P4)

- Proxy các route A0 qua gateway P4 (kiểm tra sở hữu → 404, timeout client > `timeout_s` + 10 s), scope Portal `sandboxes:files`.
- Bổ sung MCP (P6) tool `read_file`/`write_file` dùng files API thay vì exec.
- **Xong khi:** file chéo org → 404; e2e gateway ghi/đọc file; exec timeout truyền đúng.

### A2 — Model connections (cần P2; song song A0/A1)

- Migration `model_connections`, mã hoá key, RLS, API BFF + `/v1`, nút Test, chặn SSRF.
- UI Settings → Models.
- Connection nền tảng WebHub seed từ env, `flavor=webhub`, `key_mode=per_org` + bảng `model_connection_keys`; spike 4 điểm ở mục 5.2.1.
- **Xong khi:** key không bao giờ xuất hiện trong response/log (test grep); `base_url` trỏ `10.x`/`169.254.169.254` bị từ chối với connection của org; Test model chạy với server giả cả hai protocol; member không sửa được connection; với flavor webhub, request của org A luôn mang key + `X-WebHub-User` của org A (test với hub giả ghi lại header), và request có tool output luôn có `X-WebHub-Memory: 0`.

### A3 — Agent runtime + API (cần A0, A1, A2)

- `portal/internal/agent` (mục 5.3–5.6), subcommand `portal agent-worker`, bảng `agent_*`, API `/v1/.../agent/runs`, SSE, cancel, revert, checkpoint git.
- System prompt port từ upstream `agentprompt/prompt.md` (bỏ phần runtimed/`sandbox.yaml`, giữ `AGENTS.md`/`BRAIN.md`/bind `0.0.0.0`/giữ cổng).
- Test: LLM giả (server HTTP phát kịch bản stream + tool call cho cả hai protocol) + sandbox thật trên compose dev.
- **Xong khi:** kịch bản "tạo `hello.txt` rồi `cat`" chạy hết vòng với LLM giả, `files_changed = [hello.txt]`; revert xoá đúng file; hai run cùng sandbox → 409; cancel dừng ≤ 5 s; kill worker giữa run → run `failed/worker_lost` trong ≤ 90 s; SSE reconnect với `Last-Event-ID` không mất/không lặp event; run của org A không đọc được từ token org B (404).

### A4 — UI Agent + Files (cần A3)

- Tab Agent, tab Files, diff viewer, model picker (mục 5.7).
- **Xong khi:** Playwright trên compose dev với LLM giả: gửi prompt → thấy stream + thẻ tool → kết quả có diff → Revert → file biến mất; dark mode, mobile 390 px; không vượt ngân sách bundle (< 250 KB gzip, CodeMirror lazy).

### A5 — Quota, usage, MCP/SDK cho agent (cần A3, P5, P6)

- Token/chi phí vào `usage_events`; quota agent theo plan (token/tháng, số run đồng thời/org); rate limit.
- MCP tool `run_agent_task(sandbox_id, prompt)` + `get_agent_run(run_id)`; SDK `client.agent.run(...)` có iterator event.
- Webhook `agent.run.completed`.
- **Xong khi:** vượt quota → 429 `agent_quota_exceeded`; 20 run đồng thời không vượt giới hạn; hệ multi-agent mẫu (`portal/examples/agent_task.py`) giao việc và nhận kết quả chỉ qua MCP.

### A6 — Triển khai k3s + vận hành

- Deployment `portal-agent-worker` (PodSecurity restricted như PR #23), NetworkPolicy egress (sandboxd, Postgres, danh sách host LLM), Secret `PORTAL_SECRETS_KEY`, service key sandboxd riêng cho worker.
- Metrics Prometheus: `agent_runs_total{status}`, thời lượng, token, lỗi tool; log có `run_id`.
- Runbook: xoay `PORTAL_SECRETS_KEY`, xoay service key, xử lý run kẹt.
- **Xong khi:** chạy thật trên chat.devhub.io.vn với một connection (ví dụ LiteLLM/OpenRouter) làm xong một task mẫu trên sandbox thật; scale worker 0 → 2 không mất run.

### T1 — Thư viện template + tạo sandbox từ template (cần P4; song song A2/A3)

- Repo template + `template.yaml` (mục 5.8), validator manifest trong CI của repo template (template phải `install` + `build` xanh và trả 200 trên port).
- Bảng `templates`, `site_categories`; admin nền tảng publish/deprecate template; import theo tag.
- Tạo sandbox từ template: clone → render `site.config.json` + placeholders (tất định, không LLM) → chạy `install` + `dev`.
- 3 template đầu: `landing-company-nextjs`, `product-intro-nextjs`, `shop-mini-nextjs-sqlite`.
- **Xong khi:** từ API tạo được sandbox từ mỗi template với params mẫu và preview trả 200 trong ≤ 3 phút; params sai schema → 400 rõ field; `company_name` chứa `</script>`/`${process.env}` hiển thị đúng chữ, không chạy code.

### T2 — Wizard onboarding + hồ sơ khách (cần T1)

- Bảng `customer_briefs`, upload logo/ảnh (giới hạn kích thước/loại), API + BFF.
- UI wizard 4 bước (loại web → template → form từ schema → tạo), lưu nháp, trang "Hồ sơ" sửa lại được; sửa hồ sơ → ghi lại `site.config.json` qua files API (không gọi agent).
- **Xong khi:** Playwright: chọn "Landing công ty" → điền form → preview hiện đúng tên công ty/địa chỉ; sửa SĐT trên Portal → preview đổi sau reload; org B không đọc được brief org A.

### T3 — Agent dùng brief + skill (cần A3, T1)

- Ghi `.4ai/brief.json` (trong thư mục dự án), chèn tóm tắt hồ sơ vào system prompt (khối dữ liệu có đánh dấu), index skill + tool `read_skill`.
- Skill dùng chung đầu tiên: `add-page`, `change-theme`, `seo-basics`, `contact-form`.
- **Xong khi:** với LLM giả, prompt "thêm trang giới thiệu" khiến agent gọi `read_skill("add-section"/"add-page")` trước khi sửa; agent không hardcode tên công ty (test: đổi `company_name` trong brief sau run → trang mới cũng đổi theo); slogan chứa câu "ignore previous instructions" không thay đổi hành vi tool (test với kịch bản LLM giả kiểm tra prompt được bọc đúng).

### A7 — (tuỳ chọn, để sau) CLI agent trong sandbox

Chỉ làm nếu người dùng muốn dùng OpenCode/Claude Code/Codex nguyên bản. Khi đó Portal mở một **LLM proxy** (ý tưởng `authproxy` upstream) với capability token ngắn hạn gắn run, egress sandbox chỉ mở tới proxy, chạy CLI qua exec v2 `stream=ndjson`. Không làm trong giai đoạn này.

### Thứ tự

```
P1 ─▶ P2 ─┬───────────────▶ A2 ─┐
          └─▶ P3 ─▶ P4 ─▶ A1 ───┼─▶ A3 ─▶ A4 ─▶ A6
A0 (ngay, chỉ control-plane) ───┘   │      └─▶ A5 (cần P5, P6)
                  P4 ─▶ T1 ─▶ T2    └─▶ T3 (cần A3 + T1)
```

---

## 7. Ngoài phạm vi

- Agent chạy trong sandbox (A7), Claude subscription login, import OAuth của CLI.
- Terminal tương tác (xterm.js) cho người dùng.
- Git push / tạo PR từ Portal (có thể làm sau theo mô hình upstream: push ở phía control plane, PAT không vào sandbox).
- Multi-agent nội bộ (agent gọi agent con), agent "operator" kiểu Hermes.
- Tự sửa `sandbox.yaml`/supervisor (mình không có runtimed).

## 8. Câu hỏi mở (cần Doan chốt)

1. ~~Nhà cung cấp LLM mặc định~~ → **đã chốt: WebHub** (mục 5.2.1). Còn mở: cho phép org tự mang key (BYOK, LiteLLM/OpenRouter) ngay từ A2 hay để sau?
2. ~~Model mặc định~~ → `glm-4.6`. Còn mở: giới hạn free (số run/ngày, token/tháng) cho mỗi org là bao nhiêu?
3. **Worker mất lease thì fail hay resume?** Đề xuất fail (đơn giản, giống upstream); resume để sau.
4. **Image không có `git`**: chấp nhận "không revert được" hay bắt buộc image trong allowlist phải có `git` + `rg`? Đề xuất bắt buộc (thêm vào `verify-base.sh`).
5. **Agent có được tự `start` sandbox đang `stopped` và bật keep-alive trong lúc chạy không?** Đề xuất có, tắt keep-alive khi run xong.
6. **Ai được chạy agent**: mọi `member` của org, hay chỉ `admin`? Hệ multi-agent ngoài có được dùng agent (`agent:run`) hay chỉ dùng tool thô (exec/files)?
7. **Quota đầu tiên**: số run đồng thời/org và token/tháng mặc định cho plan free.
8. **Thứ tự làm**: A0 có thể bắt đầu ngay song song P3/P4 — giao cho Claude hay z.ai?
9. **PR #10 (runtime k8s)**: sandbox của Portal sẽ chạy trên k8s adapter hay docker worker? Ảnh hưởng tới cách làm exec v2/files API cho remote worker ở A0.
10. **Template lưu ở đâu**: repo riêng `4aicoding-templates` (clone khi tạo, cần egress tới GitHub lúc tạo) hay đóng sẵn vào image (tạo nhanh, không cần mạng, nhưng phải build lại image khi sửa template)? Đề xuất: repo riêng + cache tarball ở Portal.
11. **Danh mục loại web đợt đầu**: chốt 3 loại (landing công ty, giới thiệu sản phẩm, shop nhỏ) hay nhiều hơn? Và danh sách field hồ sơ bắt buộc cho từng loại.
12. **Shop nhỏ**: đặt hàng ghi SQLite qua server Next.js (cần sandbox chạy liên tục / deploy có server) hay tĩnh 100 % + đặt hàng qua Zalo/Messenger/form email?
13. **Ai viết template + skill đợt đầu**: Claude, z.ai, hay Doan có sẵn source mẫu muốn đưa vào?
14. **Key `wh_` cấp theo org hay theo project?** WebHub khuyên "1 project = 1 key". Đề xuất theo org (đơn giản, memory người dùng dùng chung giữa các project của họ); chuyển sang theo project nếu cần cách ly chặt. Và hub có API để Portal **tự tạo/thu hồi key** khi tạo/xoá org không, hay admin phải tạo tay?
15. **Gửi code và hồ sơ khách tới hub bên ngoài**: hub `space-z.ai` do ai vận hành, dữ liệu (request log, memory) giữ bao lâu? Cần ghi vào điều khoản cho khách.
