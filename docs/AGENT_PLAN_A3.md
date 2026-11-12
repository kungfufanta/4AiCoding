# KẾ HOẠCH CHI TIẾT A3 — Agent loop + tool calling + làm việc trong sandbox (ngay trong Chat)

> Trạng thái: **plan, chưa có code**. Viết ngày 2026-11-12 từ `docs/AGENT_PLAN.md` mục 5.3–5.6 và 6/A3 (PR #39), đối chiếu code `main` sau PR #64.
> Đọc kèm: `docs/AGENT_PLAN.md` (kiến trúc tổng, bảo mật 5.9), `docs/PORTAL_V2_PROGRESS.md`.
> Mỗi phase là **một PR**, có test, CI xanh. Mục "Xong khi" là tiêu chí nghiệm thu, reviewer kiểm theo đúng danh sách đó.

---

## 0. Tóm tắt

- Nâng trang **Chat hiện có** thành nơi giao việc cho agent: người dùng gắn một sandbox vào cuộc chat, bật chế độ **Agent**, gõ "thêm trang login". Không làm trang chat thứ hai.
- Vòng lặp agent chạy **trong Portal** (Go, `portal/internal/agent`). Tool đi qua `portal/internal/sandboxd` client tới sandboxd (exec v2 + files API, đã có từ A0/A1). Sandbox không có key LLM, không có agent.
- Run là **bản ghi bền trong Postgres** (`agent_runs`, `agent_events`). Worker claim run bằng `FOR UPDATE SKIP LOCKED` + lease, nên reload trang, mất kết nối, restart pod đều không làm mất trạng thái.
- Kết quả **trung thực**: `files_changed` tính từ git so với checkpoint, không tin lời LLM. Revert theo run bằng ref git riêng.
- A3 chỉ làm protocol **OpenAI-compatible** (WebHub, LiteLLM, OpenRouter, vLLM). Protocol `anthropic` để sau (A3.x hoặc A5).
- Giao diện ở A3 là bản tối thiểu trong Chat (thẻ tool call thu gọn, thẻ kết quả, Cancel/Revert). Tab Files, diff viewer, history đầy đủ để A4.

## 1. Hiện trạng (main @ `da0bb27`, 2026-11-12)

| Đã có | Ở đâu | A3 dùng thế nào |
|---|---|---|
| Exec v2: `cwd`, `env`, `stdin`, `timeout_s`, `max_output_bytes`, `timed_out`, `truncated`, stream NDJSON | sandboxd + `sandboxd.Client.Exec/ExecStream` | tool `run_command`, `search`, checkpoint git, preview check |
| Files API (list/read/write/delete/move, chặn traversal + symlink, gốc `/home/sandbox`) | sandboxd `v1_files*.go`, Portal gateway `sandboxes_v1_files.go`, `sandboxd.Client.Raw` | `list_files`, `read_file`, `write_file`, `edit_file` |
| Bảng `sandboxes` (index sở hữu theo org, RLS) | `0006_sandboxes.sql` | kiểm tra "sandbox thuộc org của run" |
| Model connections (org → platform → env), key AES-GCM, SSRF guard, Test có probe `supports_tools` | `llmconfig`, `0008`/`0009` | chọn model + key cho run |
| Client LLM streaming OpenAI, header WebHub (`X-WebHub-User`, `X-Session-Id`, `X-WebHub-Memory: 0`, `X-Request-Id`) | `portal/internal/llm` | **mở rộng**: thêm tools + `tool_calls` |
| Chat: `chat_conversations`, `chat_messages` (RLS org + user), SSE `user/delta/done/error` | `0005_chat.sql`, `httpapi/chat.go`, `features/chat` | run gắn vào conversation |
| Plans, `usage_events`, quota sandbox | `0007`, `quota`, `usage` | ghi token, chặn run vượt plan (A5 mới enforce theo tháng) |
| API key scopes `sandbox:*`, `sandboxes:files`, `usage:read`; agent token có claim `agent_run_id` | `apikeys`, `agenttoken` | thêm `agent:run`, `agent:read` |
| `PORTAL_SECRETS_KEY` tự sinh trên k3s | PR #62 | giải mã key LLM trong worker |

Chưa có: tool calling trong `llm`, bảng agent, worker, checkpoint, API run, UI Agent.

## 2. Quyết định cho 8 câu hỏi của z.ai (đã chốt 2026-11-12)

| # | Câu hỏi | Đề xuất |
|---|---|---|
| 1 | System prompt: port upstream hay viết mới | **Port `agentprompt/prompt.md` rồi sửa**: bỏ runtimed/`sandbox.yaml`; giữ bind `0.0.0.0:<port>`, tự kiểm bằng `curl`, đọc `AGENTS.md`, ghi `BRAIN.md`. Prompt nằm trong `portal/internal/agent/prompt/system.md` (go:embed), có test snapshot |
| 2 | Agent mode chọn project hay sandbox | **Chỉ sandbox, bắt buộc.** Project suy từ `sandboxes.project_id`. Một conversation gắn tối đa một sandbox (đổi sandbox thì tạo chat mới) |
| 3 | `max_steps` | Mặc định **30**, tối đa **50**. `timeout_s` mặc định **900**, tối đa **1800** |
| 4 | Worker cùng binary hay riêng | **Cùng binary.** A3 chạy worker **trong process `portal`** (goroutine, `PORTAL_AGENT_WORKERS=2`, `0` = tắt). Thêm subcommand `portal agent-worker` (chỉ chạy worker, không serve HTTP) để A6 tách Deployment. Cả hai dùng chung code |
| 5 | Token budget | A3 chỉ **giới hạn mỗi run**: `max_tokens_per_run` mặc định **300k** (vào + ra). Ghi `usage_events` kind `llm_tokens_in/out`. Quota theo tháng/plan để **A5**. Con số theo tháng (đề xuất 2M/org cho free) Doan chốt ở A5 |
| 6 | Ai được revert | **Người tạo run hoặc org admin/owner**, chỉ run `succeeded/failed/cancelled` **mới nhất** của sandbox (hoặc chuỗi liên tiếp từ cuối lên) |
| 7 | Detect build | `package.json` (script `build`), `go.mod`, `Cargo.toml`, `pyproject.toml`, `requirements.txt`. Ưu tiên lệnh khai báo trong `AGENTS.md` (khối `build:`). Không detect được → `build_status: skipped` |
| 8 | SSE: LISTEN/NOTIFY hay polling | **LISTEN/NOTIFY** trên một connection riêng (không lấy từ pool RLS), kênh chung `agent_events` với payload `run_id:seq`. Fallback poll 1 s khi LISTEN lỗi. Replay luôn đọc từ bảng, NOTIFY chỉ để đánh thức |

Các quyết định thêm ở mục 13.

## 3. Kiến trúc A3

```
Chat page (Agent mode, sandbox đã gắn)            /v1 (API key agent:run / agent token)
   │ POST /api/orgs/{org}/chats/{chat}/agent/runs     │ POST /v1/sandboxes/{id}/agent/runs
   ▼                                                  ▼
 httpapi ──INSERT agent_runs(status=queued)──▶ Postgres ◀── NOTIFY agent_runs_queued
   │ GET …/agent/runs/{run}/events (SSE, Last-Event-ID)        │
   │      ◀── replay agent_events WHERE seq > N  + LISTEN agent_events
   │                                                           ▼
   │                                    agent.Worker (goroutine × N, hoặc `portal agent-worker`)
   │                                      claim (SKIP LOCKED, lease 60 s, gia hạn 20 s)
   │                                      ├─ ensure sandbox running (+keep_alive)
   │                                      ├─ checkpoint git (exec)
   │                                      ├─ loop: llm.StreamTools ⇄ tools.Exec
   │                                      │        └─ sandboxd.Client (sandbox_id lấy từ run, không từ LLM)
   │                                      ├─ hậu kiểm: files_changed, build, preview
   │                                      └─ event done + usage_events + chat_messages(assistant)
```

Nguyên tắc (giữ từ AGENT_PLAN 3 và 5.9):

1. `sandbox_id` và `org_id` của run lấy từ DB, **không bao giờ từ tham số tool**. Không tool nào có tham số `sandbox_id`.
2. Key LLM chỉ giải mã trong RAM của worker, không ghi vào event/log.
3. Mọi thứ đọc từ sandbox, repo, web và khối memory của hub đều là **dữ liệu**. Giới hạn bằng code (tool whitelist, timeout, cap output, max_steps, token cap), không bằng lời nhắc.
4. Một sandbox chỉ có **một run active** (`queued` hoặc `running`).

## 4. Dữ liệu — migration `0010_agent.sql`

```sql
CREATE TABLE agent_runs (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id           UUID NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    sandbox_id       TEXT NOT NULL REFERENCES sandboxes(id) ON DELETE CASCADE,
    conversation_id  UUID NULL REFERENCES chat_conversations(id) ON DELETE SET NULL,
    created_by_user  UUID NULL REFERENCES users(id),
    created_by_key   TEXT NULL,                       -- api key_id khi tạo qua /v1
    prompt           TEXT NOT NULL CHECK (length(prompt) <= 32768),
    connection_id    UUID NULL REFERENCES model_connections(id) ON DELETE SET NULL,
    model            TEXT NOT NULL,
    max_steps        INT  NOT NULL DEFAULT 30  CHECK (max_steps BETWEEN 1 AND 50),
    timeout_s        INT  NOT NULL DEFAULT 900 CHECK (timeout_s BETWEEN 30 AND 1800),
    max_tokens       INT  NOT NULL DEFAULT 300000,
    status           TEXT NOT NULL DEFAULT 'queued'
                     CHECK (status IN ('queued','running','succeeded','failed','cancelled')),
    error_code       TEXT NULL,   -- worker_lost|timeout|max_steps|token_budget|llm_error|sandbox_unavailable|image_missing_tools|internal
    checkpoint_ref   TEXT NULL,   -- refs/4aicoding/checkpoints/<run_id>, NULL nếu image không có git
    result           JSONB NULL,  -- {summary, files_changed[], build_status, preview_ok, steps}
    steps            INT    NOT NULL DEFAULT 0,
    input_tokens     BIGINT NOT NULL DEFAULT 0,
    output_tokens    BIGINT NOT NULL DEFAULT 0,
    reverted_at      TIMESTAMPTZ NULL,
    cancel_requested BOOLEAN NOT NULL DEFAULT false,
    lease_owner      TEXT NULL,
    lease_until      TIMESTAMPTZ NULL,
    idempotency_key  TEXT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at       TIMESTAMPTZ NULL,
    finished_at      TIMESTAMPTZ NULL
);
CREATE UNIQUE INDEX agent_runs_one_active_per_sandbox
    ON agent_runs (sandbox_id) WHERE status IN ('queued','running');
CREATE INDEX agent_runs_queue ON agent_runs (created_at) WHERE status = 'queued';
CREATE INDEX agent_runs_sandbox_recent ON agent_runs (sandbox_id, created_at DESC);
CREATE UNIQUE INDEX agent_runs_idem ON agent_runs (org_id, idempotency_key) WHERE idempotency_key IS NOT NULL;

CREATE TABLE agent_events (
    run_id     UUID NOT NULL REFERENCES agent_runs(id) ON DELETE CASCADE,
    seq        INT  NOT NULL,
    org_id     UUID NOT NULL,
    type       TEXT NOT NULL,   -- status|message|tool_call|tool_result|build|done|reverted
    data       JSONB NOT NULL,  -- ≤ 256 KiB (cắt ở code trước khi INSERT)
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (run_id, seq)
);

CREATE TABLE agent_messages (       -- lịch sử LLM đầy đủ (kể cả tool) để "tiếp tục" trong cùng chat
    run_id  UUID NOT NULL REFERENCES agent_runs(id) ON DELETE CASCADE,
    idx     INT  NOT NULL,
    org_id  UUID NOT NULL,
    role    TEXT NOT NULL CHECK (role IN ('system','user','assistant','tool')),
    content JSONB NOT NULL,
    PRIMARY KEY (run_id, idx)
);

ALTER TABLE chat_conversations ADD COLUMN sandbox_id TEXT NULL REFERENCES sandboxes(id) ON DELETE SET NULL;
ALTER TABLE chat_messages ADD COLUMN agent_run_id UUID NULL REFERENCES agent_runs(id) ON DELETE SET NULL;
```

- RLS cho cả ba bảng agent: `org_id = app.org_id` (giống `0002`). Run là tài nguyên của org (sandbox thuộc org), còn conversation vẫn riêng tư theo user như `0005`. BFF chỉ liệt kê run của conversation mà user đọc được. `/v1` thấy mọi run của org theo scope.
- Worker chạy ngoài request, nên mỗi thao tác dùng `db.WithOrg(ctx, pool, run.OrgID, …)`. Riêng bước claim cần thấy hàng đợi của mọi org, nên dùng hàm `SECURITY DEFINER agent_claim_next(worker text)` trả về đúng một run và set lease. Hàm này là con đường cross-org duy nhất, có test riêng.
- `NOTIFY agent_events, '<run_id>:<seq>'` sau mỗi INSERT event, và `NOTIFY agent_runs_queued` khi tạo run.
- Job dọn: `agent_events` và `agent_messages` của run kết thúc hơn 30 ngày thì xoá. `agent_runs` giữ lại.

## 5. LLM client — tool calling (`portal/internal/llm`)

Thêm, không phá API `Stream` hiện tại (Chat MVP vẫn dùng):

```go
type Tool struct { Name, Description string; Parameters json.RawMessage } // JSON Schema
type ToolCall struct { ID, Name string; Arguments json.RawMessage }
type Msg struct {                 // thay Message cho agent: hỗ trợ tool
    Role       string             // system|user|assistant|tool
    Content    string
    ToolCalls  []ToolCall         // assistant
    ToolCallID string             // tool
}
type Turn struct { Text string; ToolCalls []ToolCall; FinishReason string; Usage Usage }
type Usage struct { InputTokens, OutputTokens int }

func (c *Client) StreamTools(ctx context.Context, req ToolRequest, onText func(string) error) (Turn, error)
```

- Gửi `tools` + `tool_choice: "auto"` + `stream_options: {include_usage: true}`. Ghép `delta.tool_calls[i].function.arguments` theo `index`. Không có `usage` thì ước lượng bằng `len(bytes)/4` và đánh dấu `usage_estimated`.
- **Chế độ dự phòng** khi model không hỗ trợ tool (`Model.SupportsTools == false`, hoặc upstream trả 400 khi có `tools`): system prompt thêm giao thức "trả về đúng một khối ```tool {json}```". Parser chặt: đúng một khối, JSON hợp lệ, `name` nằm trong registry, sai thì trả lỗi cho LLM như một tool_result. Chọn mode **một lần mỗi run** và ghi vào event `status{tool_mode}`.
- Retry: lỗi mạng hoặc 5xx/429 được retry tối đa 3 lần (backoff 1 s, 3 s, 9 s) với cùng `X-Request-Id = <run_id>:<step>`. Lỗi 4xx không retry.
- WebHub: mọi call từ step 2 trở đi (đã có tool output) gửi `X-WebHub-Memory: 0`. User là `u_<user_uuid>` hoặc `k_<key_id>`, session là `s_<conversation_id|run_id>`.
- **Spike bắt buộc đầu A3.0** (ghi kết quả vào mô tả PR): `glm-4.6` qua WebHub có trả `tool_calls` khi `stream:true` không, có trả `usage` không, context window thực tế. Kết quả quyết định mode mặc định cho WebHub.

**Kết quả spike (đã chạy 2026-11-12, script `scripts/a3_spike_webhub.sh` — key đọc từ env `WEBHUB_KEY`, file không chứa key):**

1. **Native tool calling hoạt động với `stream:true`**: `glm-4.6` (hub map sang `glm-4-plus`) trả `delta.tool_calls` đúng spec OpenAI — `index`, `id`, `function.name`, `function.arguments`. Thứ tự chunk: text delta trước → tool_calls delta → `finish_reason:"tool_calls"`.
2. **Parallel tool calls hoạt động**: 2 call trả trong các chunk riêng với `index:0` và `index:1`.
3. **Arguments đến nguyên một chunk** (không tách fragment) — nhưng client vẫn phải ghép fragment theo index cho các provider khác (OpenRouter/vLLM).
4. **`usage` trong stream: KHÔNG có** — kể cả khi gửi `stream_options:{include_usage:true}`. Endpoint non-stream trả `usage` đầy đủ. → Client ước lượng `len(bytes)/4` + đánh dấu `usage_estimated` (đã implement).
5. **`glm-4.5-air` cũng chấp nhận `tools`** (HTTP 200, không 400).
6. **Mode mặc định cho WebHub: native** (fallback chỉ là lưới an toàn khi gặp provider khác).
7. Context window thực tế không đọc được từ hub (`/v1/models` không trả `context_window`); dùng giá trị khai báo trong `model_connections.models` (A2), mặc định 128k cho glm-4.6.

## 6. Tool registry (`portal/internal/agent/tools`)

Đường dẫn trong tool là **tương đối với workspace** (thư mục dự án: `/home/sandbox/workspace/app` nếu tồn tại, vì gitimport clone vào đó; nếu không có thì `/home/sandbox/workspace`. Chọn một lần lúc bắt đầu run, lưu vào event `status`). Code chuẩn hoá path, chặn `..` và path tuyệt đối trước khi gọi files API (files API vẫn tự chặn lần nữa).

| Tool | Tham số | Thực thi | Giới hạn / ghi chú |
|---|---|---|---|
| `list_files` | `path`, `depth≤4` | files API list | ≤ 2 000 mục, bỏ `node_modules`, `.git`, `dist`, `.next` |
| `read_file` | `path`, `offset`, `limit≤2000` | files API read | trả kèm số dòng; file nhị phân → báo "binary, N bytes" |
| `write_file` | `path`, `content` | files API PUT | ≤ 1 MiB; tạo thư mục cha |
| `edit_file` | `path`, `old`, `new`, `replace_all` | read → thay → PUT với `if_match` | `old` không thấy hoặc không duy nhất → lỗi rõ cho LLM |
| `search` | `pattern`, `path`, `glob` | exec `rg --json -m 200` (fallback `grep -rn`) | ≤ 200 kết quả, mỗi dòng ≤ 300 ký tự |
| `run_command` | `command`, `cwd`, `timeout_s≤600` | exec v2 `bash -lc`, `max_output_bytes=262144` | output đưa lại LLM cắt còn 16 KiB (đầu 8 KiB + cuối 8 KiB) |
| `check_preview` | `path="/"`, `port?` | exec `curl -sS -o /dev/null -w '%{http_code}' --max-time 10 http://127.0.0.1:<port><path>` | port mặc định = port public đầu tiên của sandbox |
| `finish` | `summary` | kết thúc vòng lặp | summary ≤ 4 000 ký tự |

- Không có tool đổi egress, mở port, xoá/stop sandbox, đọc env nền tảng. `read_skill` để **T3**.
- Mỗi tool call ghi `audit_log` Portal (`agent.tool`, tên tool, path hoặc `argv[0]`, exit code, duration). Không ghi nội dung file hay output.
- Tool lỗi (exit ≠ 0, 4xx từ files API) **không** làm hỏng run: trả lỗi về LLM như tool_result. Run chỉ fail khi sandboxd không truy cập được 3 lần liên tiếp (`sandbox_unavailable`).

## 7. Vòng lặp (`portal/internal/agent/loop.go`)

1. **Claim**: `agent_claim_next(worker_id)` đặt `status=running`, `lease_until=now()+60s`, `started_at`. Goroutine heartbeat gia hạn lease mỗi 20 s và đọc `cancel_requested`.
2. **Chuẩn bị sandbox**: `Get`. Nếu `stopped` thì `Start` và bật `keep_alive` trong lúc chạy (tắt khi xong). Nếu không thuộc org thì `failed/sandbox_unavailable`. Kiểm tra `git --version` và `rg --version`: thiếu git thì `checkpoint_ref=NULL` và event cảnh báo (run vẫn chạy); thiếu rg thì `search` dùng grep.
3. **Checkpoint** (mục 8) → event `status{phase:"checkpoint", ref}`.
4. **Dựng context**: system prompt (embed) + khối `<workspace>` (cây thư mục rút gọn ≤ 200 dòng, `AGENTS.md` ≤ 8 KiB, `BRAIN.md` ≤ 8 KiB, mỗi khối có nhãn *untrusted data*) + tóm tắt các run trước trong cùng conversation (≤ 3 run, chỉ `summary` + `files_changed`) + prompt của người dùng.
5. **Lặp** `step = 1..max_steps`:
   - `StreamTools`: đẩy event `message{delta}` (gộp 250 ms hoặc 512 ký tự).
   - Không có tool call → kết thúc với text cuối làm summary.
   - Có tool call → validate theo JSON Schema → event `tool_call{id,name,args_preview}` → thực thi (timeout riêng) → event `tool_result{id, ok, exit_code?, output_preview, truncated}` → thêm vào messages.
   - `finish` → kết thúc.
   - Sau mỗi step: cộng token. Vượt `max_tokens` thì dừng với `failed/token_budget`. Vượt `timeout_s` thì `failed/timeout`. Có `cancel_requested` thì huỷ ctx và `cancelled`.
   - Nén context khi token > 70 % context window (nếu model không khai báo thì lấy 128k): thay các tool_result cũ hơn 6 step bằng bản tóm tắt một dòng (không gọi LLM thêm ở A3; tóm tắt bằng LLM để A5).
6. **Hậu kiểm** (cả khi fail/cancel, nếu sandbox còn sống): `files_changed` từ git; build (mục 7 bảng quyết định) với timeout 300 s → event `build{status, tail}`; `check_preview` nếu sandbox có port → `preview_ok`.
7. **Kết thúc**: event `done{status, error_code?, result}` là event cuối duy nhất. Cập nhật `agent_runs`, ghi `usage_events` (`llm_tokens_in`, `llm_tokens_out`, `agent_steps`), INSERT `chat_messages(role=assistant, content=summary, agent_run_id)`, tắt keep-alive.

**Worker mất lease**: worker nào thấy run `running` với `lease_until < now() - 30s` thì chuyển run sang `failed/worker_lost` và ghi event `done`. Không resume. Người dùng bấm "Tiếp tục" để tạo run mới trong cùng chat (prompt "tiếp tục việc dang dở", context có summary của run cũ).

**Shutdown**: SIGTERM thì worker dừng nhận run mới, cancel run đang chạy sau 20 s, ghi `failed/worker_shutdown`. Deployment cần `terminationGracePeriodSeconds: 30`.

## 8. Checkpoint, files_changed, revert

Chạy qua exec, `cwd` = thư mục dự án (mục 6), `timeout_s=60`:

```bash
set -euo pipefail
git rev-parse --is-inside-work-tree >/dev/null 2>&1 || git init -q
idx=$(mktemp); trap 'rm -f "$idx"' EXIT
export GIT_INDEX_FILE="$idx"
git add -A
tree=$(git write-tree)
p=""; head=$(git rev-parse -q --verify HEAD) && p="-p $head"
c=$(git -c user.name=4aicoding -c user.email=agent@4aicoding.local commit-tree "$tree" $p -m "checkpoint $RUN_ID")
git update-ref "refs/4aicoding/checkpoints/$RUN_ID" "$c"
echo "$c"
```

- `RUN_ID` truyền qua `env` của exec, không nội suy vào chuỗi lệnh.
- `files_changed`: dùng index tạm như trên, `git add -A && git diff --cached --name-status <ref>` → `[{path, status: A|M|D|R}]`, tối đa 500 mục.
- Revert (`POST …/runs/{id}/revert`): chỉ cho run mới nhất chưa revert của sandbox, và sandbox không có run active. Các bước: `git checkout <ref> -- .`, rồi xoá các path có status `A` trong `files_changed` của chính run đó (không dùng `git clean` toàn bộ). Sau đó event `reverted`, set `reverted_at`, và hậu kiểm lại để `files_changed` sau revert phải rỗng.
- Không đụng `HEAD`, branch hay staging của người dùng.

## 9. API

### 9.1 BFF (cookie, cho Chat)

| Route | Ghi chú |
|---|---|
| `PATCH /api/orgs/{org}/chats/{chat}` `{sandbox_id}` | gắn sandbox (phải thuộc org, chat phải chưa có run) |
| `POST /api/orgs/{org}/chats/{chat}/agent/runs` `{prompt, model?, max_steps?}` | tạo run (201). `409 run_in_progress` nếu sandbox đang có run. `400 sandbox_required`. `400 model_no_tools` nếu model chắc chắn không dùng được |
| `GET /api/orgs/{org}/chats/{chat}/agent/runs` | các run của chat |
| `GET /api/orgs/{org}/agent/runs/{run}` | chi tiết |
| `GET /api/orgs/{org}/agent/runs/{run}/events` | SSE, `id:` = seq, nhận `Last-Event-ID` hoặc `?since=`. Ping mỗi 15 s. Đóng sau `done` |
| `POST /api/orgs/{org}/agent/runs/{run}/cancel` | 202. Worker dừng trong ≤ 5 s |
| `POST /api/orgs/{org}/agent/runs/{run}/revert` | quyền theo mục 2 câu 6 |

Tất cả qua `requireMember`. Run của org khác trả 404.

### 9.2 Public `/v1` (API key / agent token)

| Route | Scope |
|---|---|
| `POST /v1/sandboxes/{id}/agent/runs` (hỗ trợ `Idempotency-Key`) | `agent:run` |
| `GET /v1/sandboxes/{id}/agent/runs` | `agent:read` |
| `GET /v1/agent/runs/{run}` | `agent:read` |
| `GET /v1/agent/runs/{run}/events` (SSE; `?follow=false` trả JSON) | `agent:read` |
| `POST /v1/agent/runs/{run}/cancel` | `agent:run` |
| `POST /v1/agent/runs/{run}/revert` | `agent:run` |

- Thêm `agent:run` và `agent:read` vào `apikeys` (UI tạo key có checkbox). Agent token có claim `agent_run_id` thì chỉ đọc được đúng run đó.
- Cập nhật `docs/openapi.yaml`. MCP tool `run_agent_task` để A5.

### 9.3 Hợp đồng event

```
status      {phase: "queued"|"starting"|"checkpoint"|"running"|"postcheck", tool_mode?, warning?}
message     {delta: "..."}                        (gộp)
tool_call   {id, name, args_preview}              (args cắt 2 KiB, không có nội dung write_file đầy đủ: chỉ path + bytes)
tool_result {id, ok, exit_code?, duration_ms, output_preview, truncated}
build       {status: passed|failed|skipped, command?, tail?}
done        {status, error_code?, result: {summary, files_changed, build_status, preview_ok, steps, input_tokens, output_tokens}}
reverted    {files_restored, files_removed}
```

`done` là event kết thúc duy nhất của một run. `reverted` có thể đến sau `done`.

## 10. UI tối thiểu trong Chat (A3.4)

- Header chat: nút **Gắn sandbox** (dropdown sandbox của org, hiện status). Khi đã gắn thì có công tắc **Chat / Agent**.
- Agent mode: gửi prompt tạo run. Luồng hiện tin nhắn stream như chat thường, tool call hiện dạng thẻ thu gọn một dòng (icon, tên tool, path/lệnh, ✓/✗, thời gian), bấm mở thì xem output preview.
- Thẻ kết quả cuối run: summary, danh sách file đổi, build, preview, token. Nút **Revert** (ConfirmDialog) nếu được phép, nút **Tiếp tục** nếu fail/cancel.
- Nút **Dừng** khi run đang chạy. SSE tự reconnect với `Last-Event-ID`. Reload trang thì dựng lại từ `GET runs` + events.
- Sandbox đang có run từ nơi khác (API) thì ô nhập bị khoá, hiện "Agent đang chạy (từ API)".
- Đạt checklist chất lượng UI của `PORTAL_PLAN_V2.md` §4 (dark mode, mobile 390 px). Bundle vẫn < 250 KB gzip.

## 11. Phases

### A3.0 — Spike + LLM tool calling (chỉ `portal/internal/llm`)

- Spike WebHub (mục 5), ghi kết quả vào PR. Không commit key.
- `StreamTools`, `Msg`/`ToolCall`/`Usage`, chế độ dự phòng JSON, retry với `X-Request-Id`.
- Fake server OpenAI trong `llm/llmtest` phát kịch bản: text thuần, tool_calls tách nhiều delta, nhiều tool call song song, `usage` cuối stream, 429 rồi OK, 400 khi có `tools`.
- **Xong khi:** test ghép đúng arguments từ ≥ 3 delta; 2 tool call song song giữ đúng thứ tự `index`; fallback parse được khối ```tool``` và từ chối khi có 2 khối hoặc tool lạ; retry giữ nguyên `X-Request-Id`; `X-WebHub-Memory: 0` có mặt khi messages chứa role `tool`; `Stream` cũ của Chat không đổi hành vi (test cũ xanh).

### A3.1 — Schema + run API + worker khung (chưa có tool thật)

- Migration `0010_agent.sql`, `agent_claim_next`, RLS, grants.
- `portal/internal/agent`: `Store` (runs/events/messages), `Worker` (claim, lease, heartbeat, cancel, worker_lost sweeper, shutdown), loop chỉ gọi LLM (không tool).
- Wiring: `PORTAL_AGENT_WORKERS` (mặc định 2, `0` = tắt), subcommand `portal agent-worker`.
- Route BFF + `/v1` tạo/xem/cancel run, SSE events với replay + LISTEN/NOTIFY.
- **Xong khi:** run với LLM giả đi `queued → running → succeeded` và có đủ event `status/message/done`; run thứ hai trên cùng sandbox → 409; cancel trong lúc LLM giả đang stream chậm dừng ≤ 5 s và trạng thái `cancelled`; dừng worker giữa run (cancel ctx không cập nhật DB, mô phỏng crash) thì sweeper đặt `failed/worker_lost` trong ≤ 90 s; SSE ngắt ở seq 5 rồi nối lại với `Last-Event-ID: 5` nhận đúng từ 6, không trùng, không thiếu; org B đọc run org A → 404 (cả BFF và `/v1`); key thiếu `agent:read` → 403.

### A3.2 — Tools + checkpoint + hậu kiểm

- Registry 8 tool (mục 6), checkpoint/files_changed (mục 8), build detect, preview check, audit `agent.tool`.
- Test chạy với **sandboxd thật** trên compose `--profile sandboxes` (job integration của CI), LLM giả theo kịch bản.
- **Xong khi:** kịch bản "write_file `hello.txt` → run_command `cat hello.txt` → finish" cho `files_changed=[{hello.txt, A}]` và tool_result chứa nội dung; `edit_file` với `old` xuất hiện 2 lần trả lỗi cho LLM, run vẫn tiếp tục; path `../../etc/passwd` và `/etc/passwd` bị chặn trước khi gọi sandboxd; `run_command "sleep 999"` với `timeout_s: 2` trả `timed_out` trong ≤ 4 s; output 5 MB chỉ đưa 16 KiB cho LLM; image không có git → run succeeded với cảnh báo, `checkpoint_ref` NULL; không có event nào chứa giá trị `SECRET_MARKER` đặt trong env của LLM giả hoặc key connection (grep toàn bộ `agent_events` + log).

### A3.3 — Revert + continue + usage

- Revert (mục 8), quyền theo câu 6. Tóm tắt run trước khi "Tiếp tục" trong cùng chat. Ghi `usage_events` cho token, steps.
- **Xong khi:** revert run vừa tạo `hello.txt` + sửa `README.md` thì `hello.txt` biến mất và `README.md` về nội dung cũ, file người dùng tạo trước run không bị đụng; revert run không phải mới nhất → 409 `not_latest_run`; member không phải người tạo và không phải admin → 403; vượt `max_tokens` → `failed/token_budget`; `usage_events` có đúng tổng token của run.

### A3.4 — Agent mode trong Chat (UI tối thiểu)

- Mục 10. Playwright trên preview stack với LLM giả (thêm service `fake-llm` vào `docker-compose.portal.yml` profile `e2e`, kịch bản cố định) và sandboxd thật.
- **Xong khi:** Playwright: gắn sandbox → bật Agent → gửi "tạo hello.txt" → thấy thẻ `write_file` và `run_command` → thẻ kết quả có `hello.txt` → Revert → kiểm tra qua files API là file đã mất; reload giữa run vẫn thấy tiến độ và kết quả cuối; nút Dừng hoạt động; ảnh chụp dark + mobile 390 px đính kèm PR; bundle < 250 KB gzip.

### A3.5 — Hardening + docs + deploy

- `docs/openapi.yaml`, `docs/portal-quickstart.md` (giao việc cho agent qua `/v1`), cập nhật `PORTAL_V2_PROGRESS.md`.
- Helm: env `PORTAL_AGENT_WORKERS`, `terminationGracePeriodSeconds: 30`. Không tách Deployment (để A6).
- Metrics log: mỗi run một dòng log kết thúc có `run_id`, `org_id`, status, steps, tokens, duration.
- **Xong khi:** trên chat.devhub.io.vn, với connection WebHub thật, giao một task mẫu ("tạo file index.html hiển thị Hello rồi kiểm tra bằng curl") trên một sandbox thật và nhận `succeeded` + `files_changed` đúng. Nếu WebHub chưa hỗ trợ tool, chạy được ở chế độ dự phòng.

Thứ tự: `A3.0 → A3.1 → A3.2 → A3.3 → A3.4 → A3.5`. A3.0 và phần migration của A3.1 có thể làm song song.

## 12. Rủi ro

| Rủi ro | Biện pháp |
|---|---|
| WebHub/glm-4.6 không trả `tool_calls` khi stream | Spike A3.0; chế độ dự phòng JSON; org có thể chọn connection khác |
| Prompt injection qua file/`AGENTS.md`/memory hub | Khối dữ liệu có nhãn; tool không nhận `sandbox_id`; không có tool nguy hiểm; `X-WebHub-Memory: 0` sau step 1 |
| Worker chết giữa run | Lease 60 s + sweeper; không resume; "Tiếp tục" |
| Hai worker claim cùng run | `SKIP LOCKED` trong `agent_claim_next` + unique partial index |
| SSE mất event | Replay từ bảng theo seq; NOTIFY chỉ để đánh thức |
| Đốt token | `max_steps`, `timeout_s`, `max_tokens` mỗi run; quota tháng ở A5 |
| Lộ secret qua event/log | Không log body LLM; event cắt 256 KiB; `tool_call` của `write_file` không chứa content; test `SECRET_MARKER` |
| Sandbox stop vì idle giữa run | Bật keep-alive lúc chạy, tắt khi xong |
| Run làm hỏng code người dùng | Checkpoint trước mỗi run; revert; không đụng HEAD/branch |
| Pool RLS bị dùng cross-org | Chỉ `agent_claim_next` (SECURITY DEFINER) thấy hàng đợi; mọi thao tác khác qua `WithOrg(run.OrgID)` |

## 13. Đã chốt (Doan giao Claude chốt, 2026-11-12)

Bảng ở mục 2 là quyết định chính thức, không còn là đề xuất. Thêm:

1. **`/v1` cho hệ multi-agent ngoài làm ngay ở A3.1** (scope `agent:run`, `agent:read`), vì dùng chung code với BFF. MCP `run_agent_task` vẫn ở A5.
2. **Thư mục dự án**: `/home/sandbox/workspace/app` nếu tồn tại (gitimport clone vào đó, xem `control-plane/internal/api/handlers.go`), nếu không thì `/home/sandbox/workspace`. Files API vẫn tính path từ `/home/sandbox`, nên tool tự thêm tiền tố.
3. **Một chat gắn cố định một sandbox.** Muốn làm việc với sandbox khác thì tạo chat mới. Chat chưa có run thì vẫn đổi được sandbox.
4. **Quota tháng (A5)**: free 2M token/org/tháng, cảnh báo 80 %, chặn 100 %. Điều chỉnh khi biết giới hạn thật của WebHub (spike A3.0 ghi lại).
5. Người làm: z.ai làm A3.0 → A3.5 theo thứ tự, mỗi phase một PR. Claude review từng PR theo mục "Xong khi".

## 14. Ngoài phạm vi A3

Protocol `anthropic`, tab Files/diff viewer/history đầy đủ (A4), quota tháng + MCP `run_agent_task` + SDK + webhook (A5), Deployment worker riêng + NetworkPolicy + metrics Prometheus (A6), `read_skill` + brief (T3), git push/PR, agent gọi agent con.
